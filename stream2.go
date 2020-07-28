package muxrpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"sync"
	"sync/atomic"

	"github.com/karrick/bufpool"
	"go.cryptoscope.co/luigi"
	"go.cryptoscope.co/muxrpc/codec"
)

// ByteSource is inspired by sql.Rows but without the Scan(), it just reads plain []bytes, one per muxrpc packet.
type ByteSource interface {
	Next(context.Context) bool // blocks until there are new muxrpc frames for this stream

	// instead of returning an (un)marshaled object
	// we just give access to the received []byte contained in the muxrpc body
	io.Reader

	// when processing fails or the context was canceled
	Err() error

	// sometimes we want to close a query early before it is drained
	// (this sends a EndErr packet back )
	Cancel(error)
}

type byteSource struct {
	bpool bufpool.FreeList
	buf   frameBuffer

	mu     sync.Mutex
	closed chan struct{}
	failed error

	requestID int32
	pkgFlag   codec.Flag

	streamCtx context.Context
	cancel    context.CancelFunc
}

func newByteSource(ctx context.Context, pool bufpool.FreeList) *byteSource {
	bs := &byteSource{
		bpool: pool,
		buf: frameBuffer{
			store: pool.Get(),
		},
		closed: make(chan struct{}),
	}
	bs.streamCtx, bs.cancel = context.WithCancel(ctx)

	return bs
}

func (bs *byteSource) Cancel(err error) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	// fmt.Println("muxrpc: byte source canceled with", err)

	if bs.failed != nil {
		// fmt.Println("muxrpc: byte source already canceld", bs.failed)
		return
	}
	// TODO: send EndErr packet back on stream
	bs.CloseWithError(err)
}

func (bs *byteSource) CloseWithError(err error) error {
	// cant lock here because we might block in next
	if err == nil {
		bs.failed = luigi.EOS{}
	} else {
		bs.failed = err
	}
	close(bs.closed)
	return nil
}

func (bs *byteSource) Close() error {
	return bs.CloseWithError(nil)
}

func (bs *byteSource) Err() error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if luigi.IsEOS(bs.failed) || errors.Is(bs.failed, context.Canceled) {
		return nil
	}

	return bs.failed
}

// TODO: might need to add size to size
func (bs *byteSource) Next(ctx context.Context) bool {
	bs.mu.Lock()
	if bs.failed != nil && bs.buf.frames == 0 {
		bs.mu.Unlock()
		bs.bpool.Put(bs.buf.store)
		return false
	}
	if bs.buf.frames > 0 {
		bs.mu.Unlock()
		return true
	}
	bs.mu.Unlock()

	select {
	case <-bs.streamCtx.Done():
		bs.failed = bs.streamCtx.Err()
		return bs.buf.Frames() > 0

	case <-ctx.Done():
		bs.failed = ctx.Err()
		return false

	case <-bs.closed:
		return bs.buf.frames > 0

	case <-bs.buf.waitForMore():
		return true
	}
}

// TODO: might not be a good iead, easy to missuse (call twice and get two packates)
func (bs *byteSource) Read(b []byte) (int, error) {
	sz, err := bs.buf.readFrame(b)
	if err != nil {
		return sz, err
	}
	return sz, nil
}

func (bs *byteSource) consume(pktLen uint32, r io.Reader) error {
	bs.mu.Lock()
	defer bs.mu.Unlock()

	if bs.failed != nil {
		return fmt.Errorf("muxrpc: byte source canceled: %w", bs.failed)
	}

	err := bs.buf.copyBody(pktLen, r)
	if err != nil {
		return err
	}

	return nil
}

// legacy stream adapter

func (bs *byteSource) AsStream() Stream {
	return &bsStream{
		source: bs,
		tipe:   json.RawMessage{},
	}
}

type bsStream struct {
	source *byteSource

	tipe interface{}

	buf [1024]byte
}

func (stream *bsStream) Next(ctx context.Context) (interface{}, error) {
	if !stream.source.Next(ctx) {
		err := stream.source.Err()
		if err == nil {
			return nil, luigi.EOS{}
		}
		return nil, fmt.Errorf("muxrcp: no more elemts from source: %w", err)
	}

	// TODO: flag is known at creation tyme and doesnt change other then end
	if stream.source.pkgFlag.Get(codec.FlagJSON) {
		tv := reflect.TypeOf(stream.tipe)
		val := reflect.New(tv).Interface()

		err := json.NewDecoder(stream.source).Decode(&val)
		if err != nil {
			return nil, fmt.Errorf("muxrcp: failed to decode json from source: %w", err)
		}
		return val, nil
	} else if stream.source.pkgFlag.Get(codec.FlagString) {
		n, err := stream.source.Read(stream.buf[:])
		if err != nil {
			return nil, err
		}
		str := string(stream.buf[:n])
		fmt.Println("Next() string:", str)
		return str, nil
	} else {
		return ioutil.ReadAll(stream.source)
	}
}

func (stream *bsStream) Pour(ctx context.Context, v interface{}) error {
	err := fmt.Errorf("muxrpc: can't pour into byte source")
	panic(err)
	return err
}

func (stream *bsStream) Close() error {
	return fmt.Errorf("muxrpc: can't close byte source?")
}

func (stream *bsStream) CloseWithError(e error) error {
	stream.source.Cancel(e)
	return nil // already closed?
}

// WithType tells the stream in what type JSON data should be unmarshalled into
func (stream *bsStream) WithType(tipe interface{}) {
	// fmt.Printf("muxrpc: chaging marshal type to %T\n", tipe)
	stream.tipe = tipe
}

// WithReq tells the stream what request number should be used for sent messages
func (stream *bsStream) WithReq(req int32) {
	// fmt.Printf("muxrpc: chaging request ID to %d\n", req)
	stream.source.requestID = req
}

// utils
type frameBuffer struct {
	mu    sync.Mutex
	store *bytes.Buffer

	waiting chan<- struct{}

	frames uint32

	lenBuf [4]byte
}

func (fw *frameBuffer) Frames() uint32 {
	return atomic.LoadUint32(&fw.frames)
}

func (fw *frameBuffer) copyBody(pktLen uint32, rd io.Reader) error {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	binary.LittleEndian.PutUint32(fw.lenBuf[:], uint32(pktLen))
	fw.store.Write(fw.lenBuf[:])

	copied, err := io.Copy(fw.store, rd)
	if err != nil {
		return err
	}

	if uint32(copied) != pktLen {
		return fmt.Errorf("frameBuffer: failed to consume whole body")
	}

	atomic.AddUint32(&fw.frames, 1)
	fmt.Println("frameWriter: stored ", fw.frames, pktLen)

	if fw.waiting != nil {
		close(fw.waiting)
		fw.waiting = nil
	}
	return nil
}

func (fw *frameBuffer) waitForMore() <-chan struct{} {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	// TODO: maybe retrn nil to signal this instead of allocating channels that are immediatly closed?
	ch := make(chan struct{})
	if fw.frames > 0 {
		close(ch)
		return ch
	}

	if fw.waiting != nil {
		panic("muxrpc: already waiting")
	}
	fw.waiting = ch

	return ch
}

func (fw *frameBuffer) readFrame(buf []byte) (int, error) {
	fw.mu.Lock()
	defer fw.mu.Unlock()

	_, err := fw.store.Read(fw.lenBuf[:])
	if err != nil {
		return 0, err
	}

	pktLen := binary.LittleEndian.Uint32(fw.lenBuf[:])

	if uint32(len(buf)) < pktLen {
		return 0, fmt.Errorf("muxrpc: buffer to small to hold frame")
	}

	rd := io.LimitReader(fw.store, int64(pktLen))

	n, err := rd.Read(buf)
	if err != nil {
		return n, err
	}

	fw.frames--
	return n, nil
}
