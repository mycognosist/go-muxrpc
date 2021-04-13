// SPDX-License-Identifier: MIT

package muxrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"

	"go.cryptoscope.co/muxrpc/v2/codec"
)

// Async does an aync call on the remote.
func (r *rpc) Async(ctx context.Context, ret interface{}, re RequestEncoding, method Method, args ...interface{}) error {
	argData, err := marshalCallArgs(args)
	if err != nil {
		return err
	}

	req := &Request{
		Type: "async",

		source: newByteSource(ctx, r.bpool),
		sink:   newByteSink(ctx, r.pkr.w),

		Method:  method,
		RawArgs: argData,
	}
	req.Stream = req.source.AsStream()

	req.sink.pkt.Flag, err = re.asCodecFlag()
	if err != nil {
		return err
	}

	if err := r.start(ctx, req); err != nil {
		return fmt.Errorf("muxrpc(%s): error sending request: %w", method, err)
	}

	if !req.source.Next(ctx) {
		err := req.source.Err()
		return fmt.Errorf("muxrpc(%s): did not receive data for request: %v", method, err)
	}

	processEntry := func(rd io.Reader) error {
		switch tv := ret.(type) {
		case *[]byte:
			if re != TypeBinary {
				return fmt.Errorf("unexpected requst encoding, need TypeBinary got %v", re)
			}
			var bs []byte
			bs, err = ioutil.ReadAll(rd)
			if err != nil {
				return fmt.Errorf("error decoding json from request source: %w", err)
			}
			*tv = bs

		case *string:
			if re != TypeString {
				return fmt.Errorf("unexpected requst encoding, need TypeString got %v", re)
			}
			var bs []byte
			bs, err = ioutil.ReadAll(rd)
			if err != nil {
				return fmt.Errorf("error decoding json from request source: %w", err)
			}
			level.Debug(r.logger).Log("asynctype", "str", "err", err, "len", len(bs))
			*tv = string(bs)

		default:
			if re != TypeJSON {
				return fmt.Errorf("unexpected requst encoding, need TypeJSON got %v for %T", re, tv)
			}
			level.Debug(r.logger).Log("asynctype", "any")
			err = json.NewDecoder(rd).Decode(ret)
			if err != nil {
				return fmt.Errorf("error decoding json from request source: %w", err)
			}
		}
		return nil
	}

	if err := req.source.Reader(processEntry); err != nil {
		srcErr := req.source.Err()
		return fmt.Errorf("muxrpc(%s): async call failed: %w (%v)", method, err, srcErr)
	}

	return nil
}

func (r *rpc) Source(ctx context.Context, re RequestEncoding, method Method, args ...interface{}) (*ByteSource, error) {
	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	encFlag, err := re.asCodecFlag()
	if err != nil {
		return nil, err
	}

	req := &Request{
		Type: "source",

		source: newByteSource(ctx, r.bpool),
		sink:   newByteSink(ctx, r.pkr.w),

		Method:  method,
		RawArgs: argData,
	}
	req.sink.pkt.Flag = req.sink.pkt.Flag.Set(encFlag)

	req.Stream = req.source.AsStream()

	if err := r.start(ctx, req); err != nil {
		return nil, fmt.Errorf("error sending request: %w", err)
	}

	return req.source, nil
}

// Sink does a sink call on the remote.
func (r *rpc) Sink(ctx context.Context, re RequestEncoding, method Method, args ...interface{}) (*ByteSink, error) {
	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, err
	}

	encFlag, err := re.asCodecFlag()
	if err != nil {
		return nil, err
	}

	req := &Request{
		Type: "sink",

		sink:   newByteSink(ctx, r.pkr.w),
		source: newByteSource(ctx, r.bpool),

		Method:  method,
		RawArgs: argData,
	}
	req.sink.pkt.Flag = req.sink.pkt.Flag.Set(encFlag).Set(codec.FlagStream)
	req.Stream = req.sink.AsStream()

	if err := r.start(ctx, req); err != nil {
		return nil, fmt.Errorf("error sending request: %w", err)
	}

	return req.sink, nil
}

// Duplex does a duplex call on the remote.
func (r *rpc) Duplex(ctx context.Context, re RequestEncoding, method Method, args ...interface{}) (*ByteSource, *ByteSink, error) {
	argData, err := marshalCallArgs(args)
	if err != nil {
		return nil, nil, err
	}

	encFlag, err := re.asCodecFlag()
	if err != nil {
		return nil, nil, err
	}

	bSrc := newByteSource(ctx, r.bpool)
	bSink := newByteSink(ctx, r.pkr.w)
	bSink.pkt.Flag = bSink.pkt.Flag.Set(encFlag).Set(codec.FlagStream)

	req := &Request{
		Type: "duplex",

		source: bSrc,
		sink:   bSink,

		Method:  method,
		RawArgs: argData,
	}

	req.Stream = &streamDuplex{bSrc.AsStream(), bSink.AsStream()}

	if err := r.start(ctx, req); err != nil {
		return nil, nil, fmt.Errorf("error sending request: %w", err)
	}

	return bSrc, bSink, nil
}

// start starts a new call by allocating a request id and sending the first packet
func (r *rpc) start(ctx context.Context, req *Request) error {
	if req.abort == nil {
		req.abort = func() {} // noop
	}

	if req.RawArgs == nil {
		req.RawArgs = []byte("[]")
	}

	var (
		first codec.Packet
		err   error

		dbg = log.With(level.Debug(r.logger),
			"call", req.Type,
			"method", req.Method.String())
	)

	func() {
		r.rLock.Lock()
		defer r.rLock.Unlock()

		first.Flag = first.Flag.Set(codec.FlagJSON)
		first.Flag = first.Flag.Set(req.Type.Flags())
		first.Body, err = json.Marshal(req)

		r.highest++
		first.Req = r.highest
		r.reqs[first.Req] = req

		req.id = first.Req
		req.sink.pkt.Req = first.Req
	}()
	if err != nil {
		dbg.Log("event", "request create failed", "err", err)
		return err
	}

	dbg = log.With(dbg, "reqID", req.id)

	err = r.pkr.w.WritePacket(&first)
	if err != nil {
		return err
	}

	dbg.Log("event", "request sent", "flag", first.Flag.String())

	return nil
}
