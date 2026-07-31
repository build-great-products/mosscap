package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf8"
)

// Codec reads and writes bounded 32-bit big-endian length-prefixed frames.
type Codec struct {
	Reader        io.Reader
	Writer        io.Writer
	MaxFrameBytes uint32
}

func NewCodec(r io.Reader, w io.Writer, max uint32) Codec {
	return Codec{Reader: r, Writer: w, MaxFrameBytes: max}
}
func (c Codec) ReadFrame() ([]byte, error) {
	if c.Reader == nil || c.MaxFrameBytes == 0 {
		return nil, fmt.Errorf("protocol: invalid codec")
	}
	var header [4]byte
	if _, err := io.ReadFull(c.Reader, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 {
		return nil, ErrEmptyFrame
	}
	if n > c.MaxFrameBytes {
		return nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(c.Reader, payload); err != nil {
		return nil, err
	}
	if !validUTF8(payload) {
		return nil, ErrInvalidUTF8
	}
	return payload, nil
}
func (c Codec) WriteFrame(payload []byte) error {
	if c.Writer == nil || c.MaxFrameBytes == 0 {
		return fmt.Errorf("protocol: invalid codec")
	}
	if len(payload) == 0 {
		return ErrEmptyFrame
	}
	if uint64(len(payload)) > uint64(c.MaxFrameBytes) {
		return ErrFrameTooLarge
	}
	if !validUTF8(payload) {
		return ErrInvalidUTF8
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(payload)))
	if err := writeAll(c.Writer, header[:]); err != nil {
		return err
	}
	return writeAll(c.Writer, payload)
}
func validUTF8(p []byte) bool { return utf8.Valid(p) }
func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}
