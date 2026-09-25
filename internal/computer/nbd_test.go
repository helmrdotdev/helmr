//go:build linux || darwin

package computer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"io"
	"net"
	"testing"
	"time"
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
	cfg, _ := localGenerationFixture(t)
	d, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	client, server := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	done := make(chan error, 1)
	go func() { defer server.Close(); done <- d.ServeNBDTransmission(t.Context(), server) }()
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
	b := bytes.Repeat([]byte{3}, 4096)
	exchange(1, 0, 0, 4096, b, 0)
	if !bytes.Equal(exchange(0, 0, 0, 4096, nil, 0), b) {
		t.Fatal("socket read differs")
	}
	exchange(3, 0, 0, 0, nil, 0)
	d.phase = func(phase string) error {
		if phase == "root-synced" {
			return errors.New("injected failure")
		}
		return nil
	}
	exchange(3, 0, 0, 0, nil, 5)
	d.phase = nil
	exchange(1, 1, 0, 1, []byte{9}, 22)
	exchange(0, 0, ^uint64(0), 1, nil, 22)
	exchange(7, 0, 0, 0, nil, 22)
	exchange(4, 0, 0, 4096, nil, 0)
	if !bytes.Equal(exchange(0, 0, 0, 4096, nil, 0), make([]byte, 4096)) {
		t.Fatal("trim not zero")
	}
	if _, e := client.Write(request(2, 0, 0, 0, nil)); e != nil {
		t.Fatal(e)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	d.Close()
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if localByte(t, reopened) != 3 {
		t.Fatal("FLUSH did not persist or DISC persisted later trim")
	}
}
func TestTransmissionRejectsOversizedAndMalformed(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	d, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, b := range [][]byte{request(1, 0, 0, nbd.MaxRequest+1, nil), make([]byte, 28)} {
		if e := d.ServeNBDTransmission(t.Context(), bytes.NewBuffer(b)); e == nil {
			t.Fatal("malformed request accepted")
		}
	}
}

func TestGenerationDeviceWritesBeyondDirtyBudget(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	cfg.DirtyBlocks = 1
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	d := generationDevice{p}
	data := bytes.Repeat([]byte{9}, 3*4096)
	if n, err := d.WriteAt(t.Context(), data, 3); err != nil || n != len(data) {
		t.Fatalf("write %d: %v", n, err)
	}
	got := make([]byte, len(data))
	if _, err := d.ReadAt(t.Context(), got, 3); err != nil || !bytes.Equal(got, data) {
		t.Fatal("write/read mismatch", err)
	}
	if err := d.Trim(t.Context(), 3, len(data)); err != nil {
		t.Fatal(err)
	}
	if err := d.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.Close()
	reopened, err := OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := reopened.ReadAt(t.Context(), got, 3); err != nil || !bytes.Equal(got, make([]byte, len(got))) {
		t.Fatal("trim not durable", err)
	}
}
