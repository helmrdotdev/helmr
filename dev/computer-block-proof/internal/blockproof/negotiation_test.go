package blockproof

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func option(code uint32, payload []byte) []byte {
	b := make([]byte, 16)
	binary.BigEndian.PutUint64(b[:8], optionMagic)
	binary.BigEndian.PutUint32(b[8:12], code)
	binary.BigEndian.PutUint32(b[12:], uint32(len(payload)))
	return append(b, payload...)
}
func TestNegotiationAndPersistentFlush(t *testing.T) {
	dir := t.TempDir()
	p := createPersistent(t, dir)
	// GO is explicitly unsupported; test its defined fallback to EXPORT_NAME.
	wire := []byte{0, 0, 0, 3}
	wire = append(wire, option(7, nil)...)
	wire = append(wire, option(1, nil)...)
	wire = append(wire, request(1, 0, 0, 3, []byte{1, 2, 3})...)
	wire = append(wire, request(3, 0, 0, 0, nil)...)
	wire = append(wire, request(2, 0, 0, 0, nil)...)
	f := &fragmentedStream{in: bytes.NewReader(wire)}
	if e := Serve(f, p); e != nil {
		t.Fatal(e)
	}
	out := f.out.Bytes()
	if len(out) != 18+20+10+32 {
		t.Fatalf("length %d", len(out))
	}
	if binary.BigEndian.Uint64(out[:8]) != 0x4e42444d41474943 || binary.BigEndian.Uint16(out[16:18]) != 3 {
		t.Fatal("bad greeting")
	}
	if binary.BigEndian.Uint64(out[18:26]) != replyMagic || binary.BigEndian.Uint32(out[30:34]) != 0x80000001 {
		t.Fatal("GO not rejected")
	}
	if binary.BigEndian.Uint16(out[46:48]) != 37 {
		t.Fatal("incorrect feature flags")
	}
	if binary.BigEndian.Uint32(out[52:56]) != 0 || binary.BigEndian.Uint32(out[68:72]) != 0 {
		t.Fatal("write/flush failed")
	}
	p.Close()
	r, e := OpenPersistent(dir, 8)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	got := make([]byte, 3)
	if _, e = r.ReadAt(got, 0); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatal("socket FLUSH did not persist")
	}
}
func TestSocketFlushFailureIsNotAcknowledged(t *testing.T) {
	p := createPersistent(t, t.TempDir())
	defer p.Close()
	p.phase = func(string) error { return errors.New("injected") }
	f := &fragmentedStream{in: bytes.NewReader(append(request(3, 0, 0, 0, nil), request(2, 0, 0, 0, nil)...))}
	if e := ServeTransmission(f, p); e != nil {
		t.Fatal(e)
	}
	if binary.BigEndian.Uint32(f.out.Bytes()[4:8]) != 5 {
		t.Fatal("failed flush acknowledged")
	}
}
func TestNegotiationBoundsAndVolatileFlags(t *testing.T) {
	_, d := setup(t, 4, 4)
	for _, flags := range []byte{1, 3} {
		wire := []byte{0, 0, 0, flags}
		wire = append(wire, option(1, nil)...)
		wire = append(wire, request(2, 0, 0, 0, nil)...)
		f := &fragmentedStream{in: bytes.NewReader(wire)}
		if e := Serve(f, d); e != nil {
			t.Fatal(e)
		}
		out := f.out.Bytes()
		want := 28
		if flags == 1 {
			want += 124
		}
		if len(out) != want || binary.BigEndian.Uint16(out[26:28]) != 33 {
			t.Fatal("volatile advertised flush or bad padding")
		}
	}
	oversized := option(1, nil)
	binary.BigEndian.PutUint32(oversized[12:], 4097)
	for _, wire := range [][]byte{[]byte{0, 0, 0, 0}, append([]byte{0, 0, 0, 3}, oversized...), append([]byte{0, 0, 0, 3}, option(1, []byte("unknown"))...)} {
		f := &fragmentedStream{in: bytes.NewReader(wire)}
		if e := Serve(f, d); e == nil {
			t.Fatal("malformed negotiation accepted")
		}
	}
}
func TestFailureAfterRootRenameCanRecoverNewRoot(t *testing.T) {
	dir := t.TempDir()
	p := createPersistent(t, dir)
	fill(t, p, 6)
	p.phase = func(s string) error {
		if s == "root-renamed" {
			return errors.New("injected directory sync boundary failure")
		}
		return nil
	}
	if e := p.Flush(); e == nil {
		t.Fatal("failed flush acknowledged")
	}
	p.Close()
	r, e := OpenPersistent(dir, 8)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	assertPersistent(t, r, 6)
}
