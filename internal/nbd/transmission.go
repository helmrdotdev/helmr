package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"syscall"
)

const MaxRequest = 1 << 20

// BlockDevice is the byte-addressable export used by the transmission server.
// Flush must durably commit prior writes before returning success.
type BlockDevice interface {
	ReadAt(context.Context, []byte, int64) (int, error)
	WriteAt(context.Context, []byte, int64) (int, error)
	Trim(context.Context, int64, int) error
	Flush(context.Context) error
}

// ServeTransmission handles one already-negotiated stream sequentially, so Flush
// cannot overtake earlier requests. Only READ, WRITE, DISC, FLUSH and TRIM are
// supported; do not advertise FUA or multi-connection support. The caller owns
// negotiation, connection cancellation/deadlines, Close and exclusive device use.
func ServeTransmission(ctx context.Context, rw io.ReadWriter, d BlockDevice, size int64) error {
	if size <= 0 {
		return errors.New("invalid export size")
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
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
		var deviceErr error
		var errno uint32
		var result []byte
		switch {
		case flags != 0:
			errno = 22
		case cmd == 2:
			if off != 0 || n != 0 {
				return errors.New("invalid disconnect request")
			}
			return nil
		case off > uint64(size) || uint64(n) > uint64(size)-off:
			errno = 22
		default:
			switch cmd {
			case 0:
				result = make([]byte, int(n))
				if count, err := d.ReadAt(ctx, result, int64(off)); err != nil || count != len(result) {
					errno = 5
					deviceErr = err
					result = nil
				}
			case 1:
				if count, err := d.WriteAt(ctx, payload, int64(off)); err != nil || count != len(payload) {
					errno = 5
					deviceErr = err
					if errors.Is(err, syscall.ENOSPC) {
						errno = 28
					}
				}
			case 3:
				if n != 0 || off != 0 {
					errno = 22
				} else {
					if err := d.Flush(ctx); err != nil {
						deviceErr = err
						errno = 5
					}
				}
			case 4:
				if err := d.Trim(ctx, int64(off), int(n)); err != nil {
					errno = 5
					deviceErr = err
					if errors.Is(err, syscall.ENOSPC) {
						errno = 28
					}
				}
			default:
				errno = 22
			}
		}
		// Fatal storage faults must reach the host owner, which stops the VM.
		// Ordinary guest I/O errors (including ENOSPC) retain NBD semantics.
		var fatal interface{ FatalDeviceError() bool }
		if errors.As(deviceErr, &fatal) && fatal.FatalDeviceError() {
			return deviceErr
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
