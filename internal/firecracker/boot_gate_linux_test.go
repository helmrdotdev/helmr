//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
	"uuid"

	fcvsock "github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/helmrdotdev/helmr/internal/bootgate"
)

func TestBootGateRequiresFreshReceiptWithoutReplayingResume(t *testing.T) {
	for _, fault := range []string{"none", "challenge", "lost-reply", "empty-random"} {
		t.Run(fault, func(t *testing.T) {
			oldDial := dialVsock
			t.Cleanup(func() { dialVsock = oldDial })
			dials := 0
			done := make(chan struct{})
			dialVsock = func(_ context.Context, _ string, port uint32, _ ...fcvsock.DialOption) (net.Conn, error) {
				dials++
				if port != bootgate.Port {
					t.Errorf("wrong bootstrap port %d", port)
				}
				host, guest := net.Pipe()
				go func() {
					defer close(done)
					defer guest.Close()
					var operation [1]byte
					if _, err := io.ReadFull(guest, operation[:]); err != nil {
						t.Error(err)
						return
					}
					if operation[0] != bootgate.Resume {
						t.Errorf("operation %d", operation[0])
						return
					}
					request, err := bootgate.ReadRequest(guest)
					if err != nil {
						t.Error(err)
						return
					}
					if time.Since(request.Time) > time.Second {
						t.Error("stale bootstrap time")
					}
					if fault == "lost-reply" {
						return
					}
					var response [bootgate.ReceiptBytes]byte
					copy(response[:], request.Challenge[:])
					copy(response[bootgate.ChallengeBytes:], uuid.NewV7().String())
					copy(response[bootgate.ChallengeBytes+36:], bytes.Repeat([]byte{7}, 32))
					if fault == "challenge" {
						response[0] ^= 1
					}
					if fault == "empty-random" {
						clear(response[bootgate.ChallengeBytes+36:])
					}
					if _, err := guest.Write(response[:]); err != nil {
						t.Error(err)
					}
				}()
				return host, nil
			}
			connector := &Connector{cfg: Config{HealthTimeout: time.Second}}
			receipt, err := connector.bootstrapGuest(t.Context(), "/unused", nil, false)
			<-done
			if (err == nil) != (fault == "none") {
				t.Fatalf("bootstrap result: %v", err)
			}
			if dials != 1 {
				t.Fatalf("resume was replayed: %d connections", dials)
			}
			if err == nil && receipt.BootID == "" {
				t.Fatal("missing fresh identity")
			}
		})
	}
}

func TestBootSeedProbeWaitsForClosedSourceConnection(t *testing.T) {
	oldDial := dialVsock
	t.Cleanup(func() { dialVsock = oldDial })
	written, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	dialVsock = func(context.Context, string, uint32, ...fcvsock.DialOption) (net.Conn, error) {
		host, guest := net.Pipe()
		go func() {
			defer guest.Close()
			var operation [1]byte
			if _, err := io.ReadFull(guest, operation[:]); err != nil {
				t.Error(err)
				return
			}
			if operation[0] != bootgate.Probe {
				t.Error("probe attempted to resume source")
			}
			_, _ = io.WriteString(guest, bootgate.Protocol)
			close(written)
			<-release
		}()
		return host, nil
	}
	connector := &Connector{cfg: Config{HealthTimeout: time.Second}}
	go func() { _, err := connector.bootstrapGuest(t.Context(), "/unused", nil, true); done <- err }()
	<-written
	select {
	case err := <-done:
		t.Fatalf("probe finished with source connection open: %v", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
