package secretproxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// The maintained transport may retry a stream explicitly declared unprocessed.
// This is distinct from an uncertain stream reset or abrupt H1 connection loss.
func TestH2ExplicitlyUnprocessedRetryUsesOneAuthorization(t *testing.T) {
	cert, _, roots := testCertificate(t, "api.github.com")
	var wireAttempts atomic.Int32
	var sockets atomic.Int32
	var peers sync.WaitGroup
	errors := make(chan error, 64)
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
		c.UpstreamTLS = &tls.Config{RootCAs: roots}
		c.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			sockets.Add(1)
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			defer listener.Close()
			client, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
			if err != nil {
				return nil, err
			}
			server, err := listener.Accept()
			if err != nil {
				client.Close()
				return nil, err
			}
			peers.Add(1)
			go func() {
				defer peers.Done()
				defer server.Close()
				server.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(server, &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
				if err := conn.HandshakeContext(ctx); err != nil {
					errors <- err
					return
				}
				preface := make([]byte, len(http2.ClientPreface))
				if _, err := io.ReadFull(conn, preface); err != nil {
					errors <- err
					return
				}
				if string(preface) != http2.ClientPreface {
					t.Error("missing H2 preface")
					return
				}
				framer := http2.NewFramer(conn, conn)
				framer.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
				if err := framer.WriteSettings(); err != nil {
					errors <- err
					return
				}
				for {
					frame, err := framer.ReadFrame()
					if err != nil {
						errors <- err
						return
					}
					headers, ok := frame.(*http2.MetaHeadersFrame)
					if !ok {
						continue
					}
					if headers.PseudoValue("authority") != "api.github.com" {
						t.Error("wrong authority")
					}
					var auth string
					for _, field := range headers.Fields {
						if field.Name == "authorization" {
							auth = field.Value
						}
					}
					if auth != "Bearer synthetic-first-token" {
						t.Error("missing authorized synthetic header")
					}
					if wireAttempts.Add(1) == 1 {
						if err := framer.WriteGoAway(0, http2.ErrCodeNo, nil); err != nil {
							errors <- err
						}
						return
					}
					var block bytes.Buffer
					encoder := hpack.NewEncoder(&block)
					encoder.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
					if err := framer.WriteHeaders(http2.HeadersFrameParam{StreamID: headers.StreamID, BlockFragment: block.Bytes(), EndHeaders: true, EndStream: true}); err != nil {
						errors <- err
					}
					return
				}
			}()
			return client, nil
		}
	})
	t.Cleanup(func() { f.proxy.Close(); peers.Wait() })
	if status := request(t, f, "https://api.github.com/", testMarker); status != 200 {
		t.Errorf("status=%d sockets=%d attempts=%d", status, sockets.Load(), wireAttempts.Load())
	}
	f.proxy.Close()
	peers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Error(err)
		}
	}
	if wireAttempts.Load() != 2 || sockets.Load() != 2 || f.resolutions.Load() != 1 {
		t.Fatalf("wire=%d sockets=%d authorizations=%d", wireAttempts.Load(), sockets.Load(), f.resolutions.Load())
	}
}
