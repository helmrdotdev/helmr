package secretproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPVersionsStreamingAndNonDefaultOrigin(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1", true: "h2"}[h2], func(t *testing.T) {
			finish := make(chan struct{})
			defer close(finish)
			f := newFixtureAt(t, 8443, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: first\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-finish:
				case <-r.Context().Done():
				}
			})
			transport := f.client.Transport.(*http.Transport)
			protocols := new(http.Protocols)
			protocols.SetHTTP1(!h2)
			protocols.SetHTTP2(h2)
			transport.Protocols = protocols
			req, _ := http.NewRequest("GET", "https://api.github.com:8443/events", nil)
			req.Header.Set("Authorization", "Bearer "+testMarker)
			response, err := f.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			want := 1
			if h2 {
				want = 2
			}
			if response.ProtoMajor != want {
				t.Fatalf("guest HTTP%d want%d", response.ProtoMajor, want)
			}
			if upstream := <-f.upstreamProtocols; upstream != 2 {
				t.Fatalf("upstream HTTP%d want2", upstream)
			}
			b := make([]byte, len("data: first\n\n"))
			if _, err := io.ReadFull(response.Body, b); err != nil || string(b) != "data: first\n\n" {
				t.Fatalf("buffered stream %s %v", b, err)
			}
		})
	}
}

func TestH2ConcurrentStreamsAndCancellation(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wait" {
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	})
	transport := f.client.Transport.(*http.Transport)
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	transport.Protocols = protocols
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", "https://api.github.com/", nil)
			req.Header.Set("Authorization", "Bearer "+testMarker)
			r, e := f.client.Do(req)
			if e != nil {
				t.Error(e)
				return
			}
			defer r.Body.Close()
			io.Copy(io.Discard, r.Body)
			if r.StatusCode != 200 || r.ProtoMajor != 2 {
				t.Errorf("status/protocol %d/%d", r.StatusCode, r.ProtoMajor)
			}
		}()
	}
	wg.Wait()
	if f.resolutions.Load() != 12 {
		t.Fatal("missing per-stream authorization", f.resolutions.Load())
	}
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/wait", nil)
	response, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = io.ReadAll(response.Body)
	response.Body.Close()
	if err == nil {
		t.Fatal("cancelled stream did not reset")
	}
	if request(t, f, "https://api.github.com/", testMarker) != 200 {
		t.Fatal("one reset killed sibling traffic")
	}
}

func TestProtectedUploadStreamsBeforeEOF(t *testing.T) {
	first := make(chan struct{})
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 5)
		if _, e := io.ReadFull(r.Body, b); e != nil {
			return
		}
		close(first)
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, "ok")
	})
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	req, _ := http.NewRequest("POST", "https://api.github.com/upload", reader)
	req.Header.Set("Authorization", "Bearer "+testMarker)
	done := make(chan error, 1)
	go func() {
		r, e := f.client.Do(req)
		if e == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		done <- e
	}()
	writer.Write([]byte("first"))
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("upload buffered until EOF")
	}
	writer.Write([]byte("last"))
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestUnrelatedTLSIsEndToEnd(t *testing.T) {
	f := newFixture(t, nil)
	response, err := f.client.Get("https://public.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if !bytes.Equal(response.TLS.PeerCertificates[0].Raw, f.upstreamCertificate) {
		t.Fatal("unrelated TLS was terminated")
	}
	if f.resolutions.Load() != 0 {
		t.Fatal("unrelated request resolved")
	}
}

func TestOriginAuthorityAndUnsupportedModes(t *testing.T) {
	for _, tc := range []struct {
		name, host, header string
		status             int
	}{
		{"cross host", "public.example", "", 421}, {"port mismatch", "api.github.com:8443", "", 421},
		{"upgrade", "api.github.com", "Upgrade", 501}, {"malformed marker", "api.github.com", "X-Bad", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			r, _ := http.NewRequest("GET", "https://api.github.com/", nil)
			r.Host = tc.host
			r.Header.Set("Authorization", "Bearer "+testMarker)
			if tc.header == "Upgrade" {
				r.Header.Set("Upgrade", "websocket")
			}
			if tc.header == "X-Bad" {
				r.Header.Set("X-Bad", MarkerPrefix+"bad")
			}
			response, err := f.client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != tc.status || f.hits.Load() != 0 || f.resolutions.Load() != 0 {
				t.Fatalf("status=%d hits=%d resolve=%d", response.StatusCode, f.hits.Load(), f.resolutions.Load())
			}
		})
	}
}

func TestProtectedUpstreamCertificateMismatchFails(t *testing.T) {
	_, _, wrong := testCertificate(t, "other.example")
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) { c.UpstreamTLS = &tls.Config{RootCAs: wrong} })
	if request(t, f, "https://api.github.com/", testMarker) != 502 || f.hits.Load() != 0 {
		t.Fatal("unverified upstream received credential")
	}
}

func TestRedirectCannotCarrySubstitutedHeader(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "https://public.example/final", 302)
			return
		}
		if strings.Contains(r.Header.Get("Authorization"), "synthetic-") {
			t.Error("real header returned to redirecting client")
		}
		io.WriteString(w, "ok")
	})
	if request(t, f, "https://api.github.com/redirect", testMarker) != 200 || f.resolutions.Load() != 1 {
		t.Fatal("redirect crossed credential authority")
	}
}

func TestCloseCancelsPendingResolve(t *testing.T) {
	entered := make(chan struct{})
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
		c.Resolve = func(ctx context.Context, _ string, _ []string) (map[string][]byte, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		r, _ := http.NewRequest("GET", "https://api.github.com/", nil)
		r.Header.Set("Authorization", "Bearer "+testMarker)
		response, _ := f.client.Do(r)
		if response != nil {
			response.Body.Close()
		}
	}()
	<-entered
	closed := make(chan struct{})
	go func() { f.proxy.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel/join resolution")
	}
	<-done
}

func TestServerFirstAndTCPHalfClose(t *testing.T) {
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, e := upstream.Accept()
		if e != nil {
			return
		}
		defer conn.Close()
		io.WriteString(conn, "greeting\n")
		body, _ := io.ReadAll(conn)
		io.WriteString(conn, "reply:"+string(body))
	}()
	p, err := New(Config{AllowedDestination: func(netip.Addr) bool { return true }, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp4", upstream.Addr().String())
	}, Certificate: func(context.Context, string) (tls.Certificate, error) {
		return tls.Certificate{}, errors.New("unexpected")
	}, Resolve: func(context.Context, string, []string) (map[string][]byte, error) {
		t.Error("non-TLS resolved")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	listener, _ := net.Listen("tcp4", "127.0.0.1:0")
	go p.Serve(&destinationListener{Listener: listener, destination: netip.MustParseAddrPort("93.184.216.34:8443")})
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	greeting, e := reader.ReadString('\n')
	if e != nil || greeting != "greeting\n" {
		t.Fatalf("server first delayed %q %v", greeting, e)
	}
	io.WriteString(conn, "request")
	conn.(*net.TCPConn).CloseWrite()
	reply, e := io.ReadAll(reader)
	if e != nil || string(reply) != "reply:request" {
		t.Fatalf("half-close lost reply %q %v", reply, e)
	}
}

func TestUnrelatedTLSSupportsMoreThan64Streams(t *testing.T) {
	f := newFixture(t, nil)
	var connections []net.Conn
	defer func() {
		for _, c := range connections {
			c.Close()
		}
	}()
	for i := 0; i < 80; i++ {
		raw, err := net.DialTimeout("tcp4", f.url.Host, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		config := f.client.Transport.(*http.Transport).TLSClientConfig.Clone()
		config.ServerName = "public.example"
		conn := tls.Client(raw, config)
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if err := conn.Handshake(); err != nil {
			raw.Close()
			t.Fatalf("ordinary connection %d refused: %v", i, err)
		}
		conn.SetDeadline(time.Time{})
		connections = append(connections, conn)
	}
	if f.resolutions.Load() != 0 {
		t.Fatal("ordinary connections asked for credentials")
	}
}

func TestProtectedPortAndOpaqueALPNSelection(t *testing.T) {
	f := newFixture(t, nil)
	if got := f.proxy.Ports(); len(got) != 1 || got[0] != 443 {
		t.Fatal(got)
	}
	f.client.Transport.(*http.Transport).TLSClientConfig.NextProtos = []string{"h2"}
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	f.client.Transport.(*http.Transport).Protocols = protocols
	response, err := f.client.Get("https://public.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.TLS.NegotiatedProtocol != "h2" || !bytes.Equal(response.TLS.PeerCertificates[0].Raw, f.upstreamCertificate) {
		t.Fatal("ordinary ALPN/certificate changed")
	}
}

func TestCloseOwnsSilentAndPartialTLSConnections(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(map[bool]string{false: "silent", true: "partial TLS"}[partial], func(t *testing.T) {
			f := newFixture(t, nil)
			conn, err := net.Dial("tcp4", f.url.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if partial {
				conn.Write([]byte{22, 3})
			}
			done := make(chan struct{})
			go func() { f.proxy.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("classification reader was not joined")
			}
			conn.SetReadDeadline(time.Now().Add(time.Second))
			_, err = conn.Read(make([]byte, 1))
			if err == nil {
				t.Fatal("silent connection survived Close")
			}
			if n, ok := err.(net.Error); ok && n.Timeout() {
				t.Fatal("silent socket leaked")
			}
		})
	}
}

func TestNoSNIAndUnprotectedPortCannotSelectSecret(t *testing.T) {
	t.Run("no SNI", func(t *testing.T) {
		f := newFixture(t, nil)
		// Numeric ServerName suppresses SNI while still requiring an exact IP SAN.
		f.client.Transport.(*http.Transport).TLSClientConfig.ServerName = "93.184.216.34"
		if request(t, f, "https://api.github.com/", testMarker) != 200 || f.resolutions.Load() != 0 || f.authed.Load() != 0 {
			t.Fatal("shared IP acquired credential authority without SNI")
		}
	})
	t.Run("same hostname other port", func(t *testing.T) {
		f := newFixtureProtocols(t, 8444, true, nil, func(c *Config) { c.Origins = []string{"https://api.github.com"} })
		if request(t, f, "https://api.github.com:8444/", testMarker) != 200 || f.resolutions.Load() != 0 || f.authed.Load() != 0 {
			t.Fatal("unprotected port acquired authority")
		}
	})
}

func TestProtectedNonHTTPALPNFailsExplicitly(t *testing.T) {
	f := newFixture(t, nil)
	raw, err := net.Dial("tcp4", f.url.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	config := f.client.Transport.(*http.Transport).TLSClientConfig.Clone()
	config.ServerName = "api.github.com"
	config.NextProtos = []string{"fixture-non-http"}
	conn := tls.Client(raw, config)
	if err := conn.Handshake(); err == nil {
		t.Fatal("non-HTTP protected protocol negotiated")
	}
	if f.resolutions.Load() != 0 {
		t.Fatal("unsupported protocol resolved")
	}
}

type fragmentedWrites struct{ net.Conn }

func (c *fragmentedWrites) Write(b []byte) (int, error) {
	total := 0
	for len(b) > 0 {
		size := len(b)
		if size > 7 {
			size = 7
		}
		n, e := c.Conn.Write(b[:size])
		total += n
		b = b[n:]
		if e != nil {
			return total, e
		}
	}
	return total, nil
}
func TestFragmentedClientHelloPreservesUnrelatedTLS(t *testing.T) {
	f := newFixture(t, nil)
	transport := f.client.Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		c, e := (&net.Dialer{}).DialContext(ctx, "tcp4", f.url.Host)
		if e != nil {
			return nil, e
		}
		return &fragmentedWrites{c}, nil
	}
	r, err := f.client.Get("https://public.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	if !bytes.Equal(r.TLS.PeerCertificates[0].Raw, f.upstreamCertificate) {
		t.Fatal("fragmented TLS changed identity")
	}
}

func TestH2UncertainStreamIsNotReplayed(t *testing.T) {
	f := newFixtureProtocols(t, 443, true, func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) })
	if request(t, f, "https://api.github.com/uncertain", testMarker) != 502 || f.hits.Load() != 1 || f.resolutions.Load() != 1 {
		t.Fatal("uncertain H2 stream was replayed")
	}
	if version := <-f.upstreamProtocols; version != 2 {
		t.Fatal("uncertain-send fixture did not use H2")
	}
}

func TestDisallowedOriginalDestinationNeverDialsOrResolves(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "8.8.8.8"} {
		t.Run(ip, func(t *testing.T) {
			policy := &Dialer{Blocked: []netip.Prefix{netip.MustParsePrefix("8.8.8.8/32")}}
			f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
				c.AllowedDestination = policy.Allowed
				c.DialContext = func(context.Context, string, string) (net.Conn, error) {
					t.Error("blocked original destination dialed")
					return nil, net.ErrClosed
				}
			})
			client, host := net.Pipe()
			defer client.Close()
			defer host.Close()
			f.proxy.serveConn(&destinationConn{Conn: host, destination: netip.AddrPortFrom(netip.MustParseAddr(ip), 443)})
			if f.resolutions.Load() != 0 {
				t.Fatal("blocked destination resolved")
			}
		})
	}
}

func TestSpeculativeSocketHandedToOnlyOneRequest(t *testing.T) {
	for _, h2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "h1", true: "h2"}[h2], func(t *testing.T) {
			var dials atomic.Int32
			f := newFixtureProtocols(t, 443, h2, nil, func(c *Config) {
				dial := c.DialContext
				c.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					dials.Add(1)
					return dial(ctx, network, address)
				}
			})
			protocols := new(http.Protocols)
			protocols.SetHTTP1(!h2)
			protocols.SetHTTP2(h2)
			f.client.Transport.(*http.Transport).Protocols = protocols
			for range 2 {
				if request(t, f, "https://api.github.com/", testMarker) != 200 {
					t.Fatal("protected request failed")
				}
			}
			if dials.Load() != 2 || f.resolutions.Load() != 2 {
				t.Fatalf("socket was wasted or authenticated connection reused: dials=%d resolves=%d", dials.Load(), f.resolutions.Load())
			}
		})
	}
}

func TestIdleSpeculativePeerCloseRedialsBeforeProtectedRequest(t *testing.T) {
	peer := make(chan net.Conn, 1)
	var dials atomic.Int32
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
		dial := c.DialContext
		c.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if dials.Add(1) == 1 {
				client, server := net.Pipe()
				peer <- server
				return client, nil
			}
			return dial(ctx, network, address)
		}
	})
	conn, err := tls.Dial("tcp4", f.url.Host, &tls.Config{ServerName: "api.github.com", RootCAs: f.client.Transport.(*http.Transport).TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	(<-peer).Close()
	// Model a guest remaining idle after its TLS handshake, allowing the
	// independent upstream read pump to observe the peer's idle timeout.
	time.Sleep(20 * time.Millisecond)
	req, _ := http.NewRequest("GET", "https://api.github.com/", nil)
	req.Header.Set("Authorization", "Bearer "+testMarker)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != 200 || dials.Load() != 2 || f.resolutions.Load() != 1 {
		t.Fatalf("status=%d dials=%d resolves=%d", response.StatusCode, dials.Load(), f.resolutions.Load())
	}
}
