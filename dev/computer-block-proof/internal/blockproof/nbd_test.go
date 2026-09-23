package blockproof

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func request(cmd, flags uint16, off uint64, n uint32, body []byte) []byte {
	b := make([]byte, 28)
	binary.BigEndian.PutUint32(b[:4], 0x25609513)
	binary.BigEndian.PutUint16(b[4:6], flags)
	binary.BigEndian.PutUint16(b[6:8], cmd)
	copy(b[8:16], []byte("handle01"))
	binary.BigEndian.PutUint64(b[16:24], off)
	binary.BigEndian.PutUint32(b[24:28], n)
	return append(b, body...)
}
func TestTransmissionSocket(t *testing.T) {
	_, d := setup(t, 4, 4)
	client, server := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() { defer server.Close(); done <- ServeTransmission(server, d) }()
	exchange := func(cmd, flags uint16, off uint64, n uint32, body []byte, want uint32) []byte {
		t.Helper()
		if _, e := client.Write(request(cmd, flags, off, n, body)); e != nil {
			t.Fatal(e)
		}
		var reply [16]byte
		if _, e := io.ReadFull(client, reply[:]); e != nil {
			t.Fatal(e)
		}
		if binary.BigEndian.Uint32(reply[:4]) != 0x67446698 || !bytes.Equal(reply[8:], []byte("handle01")) {
			t.Fatal("bad reply framing")
		}
		if got := binary.BigEndian.Uint32(reply[4:8]); got != want {
			t.Fatalf("cmd %d errno %d want %d", cmd, got, want)
		}
		if cmd == 0 && want == 0 {
			b := make([]byte, n)
			if _, e := io.ReadFull(client, b); e != nil {
				t.Fatal(e)
			}
			return b
		}
		return nil
	}
	b := bytes.Repeat([]byte{3}, BlockSize)
	exchange(1, 0, 0, BlockSize, b, 0)
	if !bytes.Equal(exchange(0, 0, 0, BlockSize, nil, 0), b) {
		t.Fatal("socket read differs")
	}
	exchange(3, 0, 0, 0, nil, 95)
	exchange(1, 1, 0, 1, []byte{9}, 95)
	exchange(0, 0, ^uint64(0), 1, nil, 22)
	exchange(7, 0, 0, 0, nil, 95)
	exchange(4, 0, 0, BlockSize, nil, 0)
	if !bytes.Equal(exchange(0, 0, 0, BlockSize, nil, 0), make([]byte, BlockSize)) {
		t.Fatal("trim not zero")
	}
	if _, e := client.Write(request(2, 0, 0, 0, nil)); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestTransmissionRejectsOversizedAndMalformed(t *testing.T) {
	_, d := setup(t, 4, 4)
	for _, b := range [][]byte{request(1, 0, 0, MaxRequest+1, nil), make([]byte, 28)} {
		if e := ServeTransmission(bytes.NewBuffer(b), d); e == nil {
			t.Fatal("malformed request accepted")
		}
	}
}

type fragmentedStream struct {
	in  *bytes.Reader
	out bytes.Buffer
}

func (f *fragmentedStream) Read(b []byte) (int, error) {
	if len(b) > 1 {
		b = b[:1]
	}
	return f.in.Read(b)
}
func (f *fragmentedStream) Write(b []byte) (int, error) { return f.out.Write(b) }
func TestFragmentedStreamAndRejectedWriteKeepFraming(t *testing.T) {
	_, d := setup(t, 2, 2)
	wire := request(1, 0, 2*BlockSize, 3, []byte{1, 2, 3})
	wire = append(wire, request(1, 0, 0, 3, []byte{4, 5, 6})...)
	wire = append(wire, request(2, 0, 0, 0, nil)...)
	f := &fragmentedStream{in: bytes.NewReader(wire)}
	if e := ServeTransmission(f, d); e != nil {
		t.Fatal(e)
	}
	b := f.out.Bytes()
	if len(b) != 32 || binary.BigEndian.Uint32(b[4:8]) != 22 || binary.BigEndian.Uint32(b[20:24]) != 0 {
		t.Fatal("reply stream lost framing")
	}
	got := make([]byte, 3)
	if _, e := d.ReadAt(got, 0); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, []byte{4, 5, 6}) {
		t.Fatal("fragmented write lost bytes")
	}
}
