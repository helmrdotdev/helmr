package blockproof

import (
	"encoding/binary"
	"errors"
	"io"
)

const MaxRequest = 1 << 20

// ServeTransmission implements only NBD's simple transmission request/reply
// framing on an already connected private stream. Persistent supports local
// FLUSH; volatile Disk rejects it. No kernel attach, FUA, structured replies
// or multi-connection claims exist.
// The caller owns the stream, its deadline, cancellation and Close.
type device interface {
	ReadAt([]byte, int64) (int, error)
	WriteAt([]byte, int64) (int, error)
	Trim(int64, int) error
	Size() int64
}
type flusher interface{ Flush() error }

func ServeTransmission(rw io.ReadWriter, d device) error {
	for {
		var hdr [28]byte
		if _, err := io.ReadFull(rw, hdr[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint32(hdr[:4]) != 0x25609513 {
			return errors.New("invalid request magic")
		}
		flags := binary.BigEndian.Uint16(hdr[4:6])
		cmd := binary.BigEndian.Uint16(hdr[6:8])
		off := binary.BigEndian.Uint64(hdr[16:24])
		n := binary.BigEndian.Uint32(hdr[24:28])
		// Close on invalid framing rather than allocating/draining an untrusted length.
		if n > MaxRequest {
			return errors.New("request too large")
		}
		var payload []byte
		if cmd == 1 {
			payload = make([]byte, int(n))
			if _, err := io.ReadFull(rw, payload); err != nil {
				return err
			}
		}
		var errno uint32
		var result []byte
		switch {
		case flags != 0:
			errno = 95
		case cmd == 2:
			return nil
		case off > uint64(d.Size()) || uint64(n) > uint64(d.Size())-off:
			errno = 22
		default:
			switch cmd {
			case 0:
				result = make([]byte, int(n))
				if _, err := d.ReadAt(result, int64(off)); err != nil {
					errno = 5
					result = nil
				}
			case 1:
				if _, err := d.WriteAt(payload, int64(off)); err != nil {
					errno = 5
					if errors.Is(err, ErrCapacity) {
						errno = 28
					}
				}
			case 3:
				if n != 0 || off != 0 {
					errno = 22
				} else if f, ok := d.(flusher); ok {
					if err := f.Flush(); err != nil {
						errno = 5
					}
				} else {
					errno = 95
				}
			case 4:
				if err := d.Trim(int64(off), int(n)); err != nil {
					errno = 22
					if errors.Is(err, ErrCapacity) {
						errno = 28
					}
				}
			default:
				errno = 95
			}
		}
		var reply [16]byte
		binary.BigEndian.PutUint32(reply[:4], 0x67446698)
		binary.BigEndian.PutUint32(reply[4:8], errno)
		copy(reply[8:], hdr[8:16])
		if err := writeAll(rw, reply[:]); err != nil {
			return err
		}
		if len(result) > 0 {
			if err := writeAll(rw, result); err != nil {
				return err
			}
		}
	}
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
