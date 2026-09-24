//go:build linux || darwin

package computer

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func generationListener(t *testing.T) net.Listener {
	t.Helper()
	// Short path also works on hosts with small Unix socket address limits.
	dir, err := os.MkdirTemp("", "nbd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "nbd"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func generationHello(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	var hello [18]byte
	if _, err := io.ReadFull(conn, hello[:]); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint64(hello[:8]) != 0x4e42444d41474943 || binary.BigEndian.Uint64(hello[8:16]) != 0x49484156454f5054 || binary.BigEndian.Uint16(hello[16:]) != 3 {
		t.Fatal("invalid greeting")
	}
}

func generationNegotiate(t *testing.T, conn net.Conn, size int64) {
	t.Helper()
	generationHello(t, conn)
	var option [20]byte
	binary.BigEndian.PutUint32(option[:4], 3)
	binary.BigEndian.PutUint64(option[4:12], 0x49484156454f5054)
	binary.BigEndian.PutUint32(option[12:16], 1)
	if _, err := conn.Write(option[:]); err != nil {
		t.Fatal(err)
	}
	var export [10]byte
	if _, err := io.ReadFull(conn, export[:]); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint64(export[:8]) != uint64(size) || binary.BigEndian.Uint16(export[8:]) != 37 {
		t.Fatal("invalid geometry/features")
	}
}

func TestGenerationNBDServerNegotiatesAndFlushes(t *testing.T) {
	cfg, _ := localGenerationFixture(t)
	p, err := CreateLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	listener := generationListener(t)
	done := make(chan error, 1)
	go func() { done <- p.ServeNBD(t.Context(), listener) }()
	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	generationNegotiate(t, conn, cfg.Base.LogicalBytes)
	for _, raw := range [][]byte{request(1, 0, 7, 1, []byte{8}), request(3, 0, 0, 0, nil)} {
		if _, err = conn.Write(raw); err != nil {
			t.Fatal(err)
		}
		var reply [16]byte
		if _, err = io.ReadFull(conn, reply[:]); err != nil {
			t.Fatal(err)
		}
		if binary.BigEndian.Uint32(reply[4:8]) != 0 || !bytes.Equal(reply[8:], []byte("handle01")) {
			t.Fatal("request failed")
		}
	}
	if _, err = conn.Write(request(2, 0, 0, 0, nil)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not join")
	}
	p.Close()
	p, err = OpenLocalGeneration(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if localByte(t, p) != 8 {
		t.Fatal("negotiated flush was not durable")
	}
}

func TestGenerationNBDServerCancellation(t *testing.T) {
	for _, phase := range []string{"accept", "negotiation", "transmission", "write-payload"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _ := localGenerationFixture(t)
			p, err := CreateLocalGeneration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			listener := generationListener(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.ServeNBD(ctx, listener) }()
			if phase != "accept" {
				conn, err := net.Dial("unix", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				if phase == "negotiation" {
					generationHello(t, conn)
				} else {
					generationNegotiate(t, conn, cfg.Base.LogicalBytes)
				}
				if phase == "write-payload" {
					if _, err = conn.Write(request(1, 0, 0, 4096, nil)); err != nil {
						t.Fatal(err)
					}
				}
			}
			cancel()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("cancel returned success")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancellation failed to join")
			}
			if _, err = net.Dial("unix", listener.Addr().String()); err == nil {
				t.Fatal("listener still open")
			}
		})
	}
}
