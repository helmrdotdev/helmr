package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type failedDisk struct {
	BlockDevice
	err error
}

func (d failedDisk) ReadAt(context.Context, []byte, int64) (int, error) { return 0, d.err }

type fatalDiskError struct{ error }

func (fatalDiskError) FatalDeviceError() bool                            { return true }
func (d failedDisk) WriteAt(context.Context, []byte, int64) (int, error) { return 0, d.err }
func (d failedDisk) Trim(context.Context, int64, int) error              { return d.err }
func (d failedDisk) Flush(context.Context) error                         { return d.err }
func TestFatalDiskReadReachesHostOwner(t *testing.T) {
	for _, cmd := range []uint16{0, 1, 3, 4} {
		for _, fatal := range []bool{false, true} {
			cause := errors.New("missing disk page")
			var failure error = cause
			if fatal {
				failure = fatalDiskError{cause}
			}
			wire := make([]byte, 28)
			binary.BigEndian.PutUint32(wire[:4], 0x25609513)
			binary.BigEndian.PutUint16(wire[6:8], cmd)
			if cmd != 3 {
				binary.BigEndian.PutUint32(wire[24:], 4096)
			}
			if cmd == 1 {
				wire = append(wire, make([]byte, 4096)...)
			}
			stream := &negotiationStream{Reader: bytes.NewReader(wire)}
			err := ServeTransmission(t.Context(), stream, failedDisk{err: failure}, 4096)
			if fatal {
				if err != failure || stream.output.Len() != 0 {
					t.Fatalf("fatal swallowed: %v", err)
				}
			} else {
				if !errors.Is(err, io.EOF) || stream.output.Len() != 16 || binary.BigEndian.Uint32(stream.output.Bytes()[4:8]) != 5 {
					t.Fatalf("ordinary I/O semantics changed: %v", err)
				}
			}
		}
	}
}
