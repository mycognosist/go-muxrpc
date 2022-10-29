// SPDX-FileCopyrightText: 2021 Henry Bubert
//
// SPDX-License-Identifier: MIT

package codec

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"sync"
)

type Writer struct {
	mu sync.Mutex

	w io.Writer
}

// NewWriter creates a new packet-stream writer
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// WritePacket creates an header for the Packet and writes it and the body to the underlying writer
func (w *Writer) WritePacket(r Packet) error {
	bodyLen := len(r.Body)
	if uint(bodyLen) > math.MaxUint32 {
		return fmt.Errorf("pkt-codec: body too large (%d)", bodyLen)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	hdr := Header{
		Flag: r.Flag,
		Len:  uint32(bodyLen),
		Req:  r.Req,
	}

	if err := binary.Write(w.w, binary.BigEndian, hdr); err != nil {
		return fmt.Errorf("pkt-codec: header write failed: %w", err)
	}

	if _, err := w.w.Write(r.Body); err != nil {
		return fmt.Errorf("pkt-codec: body write failed: %w", err)
	}

	return nil
}

// Close sends 9 zero bytes and also closes it's underlying writer if it is also an io.Closer
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	_, err := w.w.Write([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0})
	if err != nil {
		return fmt.Errorf("pkt-codec: failed to write Close() packet: %w", err)
	}

	if c, ok := w.w.(io.Closer); ok {
		if err := c.Close(); err != nil {
			return fmt.Errorf("pkt-codec: failed to close underlying writer: %w", err)
		}
	}

	return nil
}
