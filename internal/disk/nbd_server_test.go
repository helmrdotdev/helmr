//go:build linux || darwin

package disk

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVersionDeviceUnalignedRequestsSurviveReopen(t *testing.T) {
	for _, budget := range []int{1, 8, 256} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			cfg.DirtyBlocks = budget
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			device := versionDevice{p}
			want := make([]byte, 12*4096)
			copy(want, bytes.Repeat([]byte{1}, 4096))
			body := bytes.Repeat([]byte{9}, 9*4096+11)
			if n, err := device.WriteAt(t.Context(), body, 4093); err != nil || n != len(body) {
				t.Fatalf("write: %d %v", n, err)
			}
			copy(want[4093:], body)
			const trimStart, trimLength = 4101, 6*4096 + 5
			if err := device.Trim(t.Context(), trimStart, trimLength); err != nil {
				t.Fatal(err)
			}
			clear(want[trimStart : trimStart+trimLength])
			if err := device.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			got := make([]byte, len(want))
			if n, err := reopened.ReadAt(t.Context(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, want) {
				t.Fatalf("reopened request boundaries differ: %d %v", n, err)
			}
		})
	}
}

func versionListener(t *testing.T) net.Listener {
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

func versionHello(t *testing.T, conn net.Conn) {
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

func versionNegotiate(t *testing.T, conn net.Conn, size int64) {
	t.Helper()
	versionHello(t, conn)
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

func TestVersionNBDServerNegotiatesAndFlushes(t *testing.T) {
	cfg, _ := localVersionFixture(t)
	p, err := CreateLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	listener := versionListener(t)
	done := make(chan error, 1)
	go func() { done <- p.ServeNBD(t.Context(), listener) }()
	conn, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	versionNegotiate(t, conn, cfg.Base.LogicalBytes)
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
	p, err = OpenLocalVersion(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if localByte(t, p) != 8 {
		t.Fatal("negotiated flush was not durable")
	}
}

func TestVersionNBDServerCancellation(t *testing.T) {
	for _, phase := range []string{"accept", "negotiation", "transmission", "write-payload"} {
		t.Run(phase, func(t *testing.T) {
			cfg, _ := localVersionFixture(t)
			p, err := CreateLocalVersion(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			listener := versionListener(t)
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
					versionHello(t, conn)
				} else {
					versionNegotiate(t, conn, cfg.Base.LogicalBytes)
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
