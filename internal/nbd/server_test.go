package nbd

import (
	"bytes"
	"encoding/binary"
	"testing"
)

type negotiationStream struct {
	*bytes.Reader
	output bytes.Buffer
}

func (s *negotiationStream) Write(b []byte) (int, error) { return s.output.Write(b) }
func negotiationOption(code, size uint32, payload []byte) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[:8], optionMagic)
	binary.BigEndian.PutUint32(b[8:12], code)
	binary.BigEndian.PutUint32(b[12:], size)
	return append(b, payload...)
}
func TestNegotiationRejectsBeforeDeviceAccess(t *testing.T) {
	cases := [][]byte{
		{0, 0, 0, 0}, {0, 0, 0, 7},
		append([]byte{0, 0, 0, 3}, negotiationOption(1, 4097, nil)...),
		append([]byte{0, 0, 0, 3}, negotiationOption(1, 1, []byte{'x'})...),
		append([]byte{0, 0, 0, 3}, negotiationOption(2, 1, []byte{'x'})...),
	}
	for _, wire := range cases {
		stream := &negotiationStream{Reader: bytes.NewReader(wire)}
		if err := negotiate(t.Context(), stream, nil, 4096); err == nil {
			t.Fatal("invalid negotiation accepted")
		}
	}
}
func TestNegotiationUnsupportedAndAbort(t *testing.T) {
	wire := append([]byte{0, 0, 0, 3}, negotiationOption(7, 0, nil)...)
	wire = append(wire, negotiationOption(2, 0, nil)...)
	stream := &negotiationStream{Reader: bytes.NewReader(wire)}
	if err := negotiate(t.Context(), stream, nil, 4096); err != nil {
		t.Fatal(err)
	}
	out := stream.output.Bytes()
	if len(out) != 58 || binary.BigEndian.Uint64(out[18:26]) != replyMagic || binary.BigEndian.Uint32(out[30:34]) != 0x80000001 || binary.BigEndian.Uint32(out[50:54]) != 1 {
		t.Fatal("bad option replies")
	}
	wire = []byte{0, 0, 0, 3}
	for range 16 {
		wire = append(wire, negotiationOption(7, 0, nil)...)
	}
	stream = &negotiationStream{Reader: bytes.NewReader(wire)}
	if err := negotiate(t.Context(), stream, nil, 4096); err == nil {
		t.Fatal("unbounded options accepted")
	}
}
