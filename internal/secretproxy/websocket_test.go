package secretproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const websocketKey = "dGhlIHNhbXBsZSBub25jZQ=="

func websocketHeaders() http.Header {
	return http.Header{"Connection": {"keep-alive, Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {websocketKey}, "Authorization": {"Bearer " + testMarker}}
}

func openWebSocket(t *testing.T, f *fixture, header http.Header, early string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	cfg := f.client.Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.ServerName = "api.github.com"
	cfg.NextProtos = []string{"http/1.1"}
	c, err := tls.Dial("tcp4", f.url.Host, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	var request strings.Builder
	request.WriteString("GET /socket HTTP/1.1\r\nHost: api.github.com\r\n")
	header.Write(&request)
	request.WriteString("\r\n" + early)
	if header.Get("Transfer-Encoding") == "chunked" {
		request.WriteString("0\r\n\r\n")
	}
	if _, err := io.WriteString(c, request.String()); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(c)
	response, err := http.ReadResponse(reader, &http.Request{Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	return c, reader, response
}

func echoWebSocket(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if r.ProtoMajor != 1 || r.Header.Get("Authorization") != "Bearer synthetic-first-token" {
		t.Error("wrong upstream protocol or authorization")
	}
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	defer c.Close()
	fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\nwelcome", websocketAccept(r.Header.Get("Sec-WebSocket-Key")))
	rw.Flush()
	io.Copy(c, rw)
}

func TestWebSocketDuplexBufferedBytesReconnectAndShutdown(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) { echoWebSocket(t, w, r) })
	c, reader, response := openWebSocket(t, f, websocketHeaders(), "early")
	if response.StatusCode != 101 {
		t.Fatal(response.Status)
	}
	received := make([]byte, len("welcomeearly"))
	if _, err := io.ReadFull(reader, received); err != nil || string(received) != "welcomeearly" {
		t.Fatalf("buffered bytes %q: %v", received, err)
	}
	for _, data := range []string{"first operation", "second operation"} {
		io.WriteString(c, data)
		received = make([]byte, len(data))
		if _, err := io.ReadFull(reader, received); err != nil || string(received) != data {
			t.Fatalf("duplex %q %v", received, err)
		}
	}
	if len(f.proxy.activeRequests) != 1 || len(f.proxy.captured) != 1 || len(f.proxy.requests) != 0 || f.resolutions.Load() != 1 {
		t.Fatal("relay lifetime or per-handshake resolution")
	}
	c.Close()
	awaitResource(t, func() bool { return len(f.proxy.activeRequests) == 0 && len(f.proxy.captured) == 0 })
	c, reader, response = openWebSocket(t, f, websocketHeaders(), "")
	if response.StatusCode != 101 || f.resolutions.Load() != 2 {
		t.Fatal("reconnect bypassed handshake")
	}
	received = make([]byte, len("welcome"))
	io.ReadFull(reader, received)
	f.mu.Lock()
	f.available = false
	f.mu.Unlock()
	denied, _, rejection := openWebSocket(t, f, websocketHeaders(), "")
	denied.Close()
	if rejection.StatusCode != 502 || f.hits.Load() != 2 {
		t.Fatal("authority loss admitted new handshake")
	}
	done := make(chan struct{})
	go func() { f.proxy.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not join relay")
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("shutdown left socket open")
	}
	if len(f.proxy.activeRequests) != 0 || len(f.proxy.captured) != 0 {
		t.Fatal("shutdown retained capacity")
	}
	f.proxy.mu.Lock()
	count := len(f.proxy.connections)
	f.proxy.mu.Unlock()
	if count != 0 {
		t.Fatal("shutdown retained sockets", count)
	}
}

func TestWebSocketHandshakeRejections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(http.Header)
	}{
		{"duplicate upgrade", func(h http.Header) { h.Add("Upgrade", "websocket") }},
		{"duplicate key", func(h http.Header) { h.Add("Sec-WebSocket-Key", websocketKey) }},
		{"duplicate version", func(h http.Header) { h.Add("Sec-WebSocket-Version", "13") }},
		{"chunked empty body", func(h http.Header) { h.Set("Transfer-Encoding", "chunked") }},
		{"hop nominated key", func(h http.Header) { h.Set("Connection", "Upgrade, Sec-WebSocket-Key") }},
		{"hop nominated protocol", func(h http.Header) {
			h.Set("Connection", "Upgrade, Sec-WebSocket-Protocol")
			h.Set("Sec-WebSocket-Protocol", "test")
		}},
		{"other upgrade", func(h http.Header) { h.Set("Upgrade", "other") }},
		{"missing connection", func(h http.Header) { h.Del("Connection") }},
		{"wrong version", func(h http.Header) { h.Set("Sec-WebSocket-Version", "12") }},
		{"invalid key", func(h http.Header) { h.Set("Sec-WebSocket-Key", "bad") }},
		{"protocol marker", func(h http.Header) { h.Set("Sec-WebSocket-Protocol", testMarker) }},
		{"extension marker", func(h http.Header) { h.Set("Sec-WebSocket-Extensions", testMarker) }},
		{"custom negotiation marker", func(h http.Header) { h.Set("Sec-WebSocket-Custom", testMarker) }},
		{"hop nominated auth", func(h http.Header) { h.Set("Connection", "Upgrade, Authorization") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			h := websocketHeaders()
			tc.change(h)
			c, _, response := openWebSocket(t, f, h, "")
			c.Close()
			if response.StatusCode < 400 || f.resolutions.Load() != 0 || f.hits.Load() != 0 {
				t.Fatalf("status=%d resolves=%d upstream=%d", response.StatusCode, f.resolutions.Load(), f.hits.Load())
			}
		})
	}
}

func TestWebSocketUpstreamRefusalAndInvalidUpgrade(t *testing.T) {
	for _, status := range []int{401, 302, 101} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
				if status == 101 {
					w.Header().Set("Upgrade", "other")
					w.Header().Set("Connection", "Upgrade")
				}
				if status == 302 {
					w.Header().Set("Location", "https://public.example/")
				}
				w.WriteHeader(status)
				if status != 101 {
					io.WriteString(w, "refused")
				}
			})
			c, _, response := openWebSocket(t, f, websocketHeaders(), "")
			defer c.Close()
			want := status
			if status == 101 {
				want = 502
			}
			if response.StatusCode != want || f.hits.Load() != 1 {
				t.Fatalf("status %d hits %d", response.StatusCode, f.hits.Load())
			}
			awaitResource(t, func() bool { return len(f.proxy.activeRequests) == 0 })
		})
	}
}

func TestWebSocketUpstreamCloseReleasesAdmission(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", websocketAccept(websocketKey))
		rw.Flush()
		<-release
	})
	// Bound only the instance fixture; no live request has begun yet.
	f.proxy.activeRequests = make(chan struct{}, 1)
	c, reader, response := openWebSocket(t, f, websocketHeaders(), "")
	defer c.Close()
	if response.StatusCode != 101 {
		t.Fatal(response.Status)
	}
	second, _, overload := openWebSocket(t, f, websocketHeaders(), "")
	second.Close()
	if overload.StatusCode != 503 || f.resolutions.Load() != 1 {
		t.Fatal("overload resolved or forwarded")
	}
	release <- struct{}{}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("upstream EOF left client open")
	}
	awaitResource(t, func() bool { return len(f.proxy.activeRequests) == 0 && len(f.proxy.captured) == 0 })
}

func TestOrdinaryHTTPRejectsUnsolicitedUpgrade(t *testing.T) {
	f := newFixtureProtocols(t, 443, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "Upgrade")
		w.Header().Set("Upgrade", "websocket")
		w.WriteHeader(101)
	})
	if request(t, f, "https://api.github.com/", testMarker) != 502 {
		t.Fatal("unsolicited upgrade accepted")
	}
}

// Retain a completed socket read until the fixture permits its pump to exit.
// This distinguishes cancellation/Close from owner completion.
type delayedWebSocketRead struct {
	net.Conn
	hold    *atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *delayedWebSocketRead) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	if err != nil && c.hold.Load() {
		c.once.Do(func() { close(c.entered) })
		<-c.release
	}
	return n, err
}

func TestWebSocketShutdownJoinsBothPumpsBeforeReleasingCapacity(t *testing.T) {
	var hold atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	f := newFixtureProtocols(t, 443, true, func(w http.ResponseWriter, r *http.Request) { echoWebSocket(t, w, r) }, func(cfg *Config) {
		dial := cfg.DialContext
		cfg.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			c, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			return &delayedWebSocketRead{Conn: c, hold: &hold, entered: entered, release: release}, nil
		}
	})
	c, reader, response := openWebSocket(t, f, websocketHeaders(), "")
	if response.StatusCode != 101 {
		t.Fatal(response.Status)
	}
	welcome := make([]byte, len("welcome"))
	if _, err := io.ReadFull(reader, welcome); err != nil {
		t.Fatal(err)
	}
	hold.Store(true)
	c.Close()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("second pump did not reach termination gate")
	}
	done := make(chan struct{})
	go func() { f.proxy.Close(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown returned before pump exit")
	default:
	}
	if len(f.proxy.activeRequests) != 1 || len(f.proxy.captured) != 1 {
		t.Fatal("capacity released before pump exit")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown failed to join")
	}
	if len(f.proxy.activeRequests) != 0 || len(f.proxy.captured) != 0 {
		t.Fatal("joined owner retained capacity")
	}
}
