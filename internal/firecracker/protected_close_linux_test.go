//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/secretproxy"
)

func TestCloseFencesProtectedTrafficWhenCleanupFails(t *testing.T) {
	for _, scenario := range []string{"canceled-barrier", "checkpoint-discard"} {
		t.Run(scenario, func(t *testing.T) {
			proxy, err := secretproxy.New(secretproxy.Config{
				Certificate:        func(context.Context, string) (tls.Certificate, error) { return tls.Certificate{}, errors.New("unused") },
				Resolve:            func(context.Context, string, []string) (map[string][]byte, error) { return nil, errors.New("unused") },
				AllowedDestination: func(netip.Addr) bool { return false },
				DialContext:        (&net.Dialer{}).DialContext,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = proxy.Close() })
			machine := &guestMachine{networkBinding: &installedNetworkBinding{secretProxy: proxy}}
			ctx := context.Background()
			var expected error
			if scenario == "canceled-barrier" {
				unlock, err := machine.lockComputer(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			} else {
				directory := t.TempDir()
				if err := os.WriteFile(filepath.Join(directory, "retained"), nil, 0600); err != nil {
					t.Fatal(err)
				}
				machine.checkpointHold = &checkpointCapture{machine: machine}
				machine.checkpointHold.artifact.VMState.Path = directory
				expected = syscall.ENOTEMPTY
			}
			if err := machine.Close(ctx); !errors.Is(err, expected) {
				t.Fatalf("Close = %v, want %v", err, expected)
			}

			// A failed disk-barrier wait must not leave credential mediation usable.
			listener := &closedProxyProbe{}
			if err := proxy.Serve(listener); !errors.Is(err, net.ErrClosed) {
				t.Fatalf("protected proxy still admits Serve after Close: %v", err)
			}
		})
	}
}

type closedProxyProbe struct{}

func (*closedProxyProbe) Accept() (net.Conn, error) { return nil, errors.New("proxy remains open") }
func (*closedProxyProbe) Close() error              { return nil }
func (*closedProxyProbe) Addr() net.Addr            { return &net.TCPAddr{} }

func TestCloseDrainsWebSocketBeforeComputerBarrier(t *testing.T) {
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-token" {
			t.Error("missing protected authorization")
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer close(upstreamClosed)
		defer conn.Close()
		fmt.Fprint(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=\r\n\r\nready")
		if err := rw.Flush(); err != nil {
			t.Error(err)
			return
		}
		_, _ = io.Copy(io.Discard, rw)
	}))
	defer upstream.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	name := upstream.Certificate().DNSNames[0]
	authority := net.JoinHostPort(name, strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	marker := secretproxy.MarkerPrefix + strings.Repeat("a", 64)
	proxy, err := secretproxy.New(secretproxy.Config{
		Origins:     []string{"https://" + authority},
		Certificate: func(context.Context, string) (tls.Certificate, error) { return upstream.TLS.Certificates[0], nil },
		Resolve: func(context.Context, string, []string) (map[string][]byte, error) {
			return map[string][]byte{marker: []byte("synthetic-token")}, nil
		},
		AllowedDestination: func(netip.Addr) bool { return true },
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Listener.Addr().String())
		},
		UpstreamTLS: &tls.Config{RootCAs: pool},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	served := make(chan error, 1)
	go func() { served <- proxy.Serve(listener) }()
	defer func() {
		proxy.Close()
		if err := <-served; err != nil {
			t.Error(err)
		}
	}()
	conn, err := tls.Dial("tcp4", listener.Addr().String(), &tls.Config{RootCAs: pool, ServerName: name, NextProtos: []string{"http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nAuthorization: Bearer %s\r\n\r\n", authority, marker)
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 101 {
		t.Fatal(response.Status)
	}
	ready := make([]byte, 5)
	if _, err := io.ReadFull(reader, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("upgraded stream: %q %v", ready, err)
	}
	machine := &guestMachine{networkBinding: &installedNetworkBinding{secretProxy: proxy}}
	unlock, err := machine.lockComputer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := make(chan error, 1)
	go func() { closed <- machine.Close(ctx) }()
	select {
	case <-upstreamClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream survived terminal close behind Computer barrier")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("guest socket survived terminal close")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close bypassed held disk barrier: %v", err)
	default:
	}
	cancel()
	select {
	case err := <-closed:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join proxy shutdown")
	}
}
