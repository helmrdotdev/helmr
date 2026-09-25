package nbd

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

type fragmentedTransmission struct{ negotiationStream }

func (s *fragmentedTransmission) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return s.Reader.Read(b)
}

type framingDisk struct {
	BlockDevice
	data   []byte
	writes int
}

func (d *framingDisk) WriteAt(_ context.Context, b []byte, offset int64) (int, error) {
	d.writes++
	return copy(d.data[offset:], b), nil
}

func TestFragmentedRejectedWritePreservesFraming(t *testing.T) {
	request := func(cmd uint16, offset uint64, body []byte) []byte {
		header := make([]byte, 28)
		binary.BigEndian.PutUint32(header, 0x25609513)
		binary.BigEndian.PutUint16(header[6:], cmd)
		binary.BigEndian.PutUint64(header[16:], offset)
		binary.BigEndian.PutUint32(header[24:], uint32(len(body)))
		return append(header, body...)
	}
	wire := request(1, 8192, []byte{1, 2, 3})
	wire = append(wire, request(1, 0, []byte{4, 5, 6})...)
	wire = append(wire, request(2, 0, nil)...)
	stream := &fragmentedTransmission{negotiationStream{Reader: bytes.NewReader(wire)}}
	disk := &framingDisk{data: make([]byte, 8192)}
	if err := ServeTransmission(t.Context(), stream, disk, int64(len(disk.data))); err != nil {
		t.Fatal(err)
	}
	reply := stream.output.Bytes()
	if len(reply) != 32 || binary.BigEndian.Uint32(reply[4:8]) != 22 || binary.BigEndian.Uint32(reply[20:24]) != 0 {
		t.Fatal("rejected write lost reply framing")
	}
	if disk.writes != 1 || !bytes.Equal(disk.data[:3], []byte{4, 5, 6}) {
		t.Fatal("valid write was not preserved")
	}
}
