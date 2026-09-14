package secretproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func awaitResource(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("resource lifetime did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCapturedAdmissionPartialHelloReleaseAndIsolation(t *testing.T) {
	var dials atomic.Int32
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
		dial := c.DialContext
		c.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			dials.Add(1)
			return dial(ctx, network, address)
		}
	})
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for range maxCapturedConnections {
		c, err := net.Dial("tcp4", f.url.Host)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		// A TLS record header without its payload forces bounded classification.
		if _, err := c.Write([]byte{22, 3, 3, 0, 128}); err != nil {
			t.Fatal(err)
		}
	}
	awaitResource(t, func() bool { return dials.Load() == maxCapturedConnections })
	excess, err := net.Dial("tcp4", f.url.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer excess.Close()
	excess.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	_, err = excess.Read(b[:])
	if err == nil {
		t.Fatal("excess captured socket remained usable")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("overload queued socket instead of closing")
	}
	if dials.Load() != maxCapturedConnections {
		t.Fatal("overload allocated speculative upstream")
	}
	// Admission is runtime-owned: another live runtime still mediates requests.
	other := newFixture(t, nil)
	if request(t, other, "https://api.github.com/", testMarker) != 200 {
		t.Fatal("one runtime exhausted another")
	}
	held[0].Close()
	awaitResource(t, func() bool { return len(f.proxy.captured) < maxCapturedConnections })
	if request(t, f, "https://api.github.com/", testMarker) != 200 {
		t.Fatal("peer close did not release admission")
	}
	done := make(chan struct{})
	go func() { f.proxy.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fence failed with partial ClientHellos")
	}
	if len(f.proxy.captured) != 0 || len(f.proxy.activeRequests) != 0 {
		t.Fatal("fence retained admission")
	}
	f.proxy.mu.Lock()
	count := len(f.proxy.connections)
	f.proxy.mu.Unlock()
	if count != 0 {
		t.Fatal("fence retained owned sockets", count)
	}
}

func TestProtectedRequestOverloadStreamingReleaseAndFence(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wait" {
			io.WriteString(w, "open\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		io.WriteString(w, "ok")
	})
	f.client.Timeout = 30 * time.Second
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	f.client.Transport.(*http.Transport).Protocols = protocols
	var responses []*http.Response
	var cancels []context.CancelFunc
	defer func() {
		for _, cancel := range cancels {
			cancel()
		}
		for _, r := range responses {
			r.Body.Close()
		}
	}()
	for range maxProtectedRequests {
		ctx, cancel := context.WithCancel(t.Context())
		cancels = append(cancels, cancel)
		r, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/wait", nil)
		r.Header.Set("Authorization", "Bearer "+testMarker)
		response, err := f.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		responses = append(responses, response)
		if response.StatusCode != 200 || response.ProtoMajor != 2 {
			t.Fatalf("stream status/protocol %d/%d", response.StatusCode, response.ProtoMajor)
		}
	}
	if len(f.proxy.requests) != 0 {
		t.Fatal("streaming held Resolve capacity")
	}
	before := f.resolutions.Load()
	if request(t, f, "https://api.github.com/", testMarker) != 503 || f.resolutions.Load() != before || f.hits.Load() != maxProtectedRequests {
		t.Fatal("overload performed protected work")
	}
	cancels[0]()
	responses[0].Body.Close()
	awaitResource(t, func() bool { return len(f.proxy.activeRequests) == maxProtectedRequests-1 })
	if request(t, f, "https://api.github.com/", testMarker) != 200 {
		t.Fatal("cancelled stream did not release request admission")
	}
	done := make(chan struct{})
	go func() { f.proxy.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream fence stalled")
	}
	if len(f.proxy.activeRequests) != 0 {
		t.Fatal("stream fence retained requests")
	}
}

func TestResolveSaturationCancellationAndFence(t *testing.T) {
	entered := make(chan string, maxProtectedRequests*2)
	var active atomic.Int32
	var peak atomic.Int32
	release := make(chan struct{})
	f := newFixtureProtocols(t, 443, true, nil, func(c *Config) {
		c.Resolve = func(ctx context.Context, _ string, selectors []string) (map[string][]byte, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old; old = peak.Load() {
				if peak.CompareAndSwap(old, n) {
					break
				}
			}
			entered <- selectors[0]
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
				return map[string][]byte{selectors[0]: []byte("synthetic-first-token")}, nil
			}
		}
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	f.client.Transport.(*http.Transport).Protocols = protocols
	f.client.Timeout = 30 * time.Second
	done := make(chan error, maxProtectedRequests)
	cancels := make([]context.CancelFunc, maxProtectedRequests)
	start := func(i int) {
		ctx, cancel := context.WithCancel(t.Context())
		cancels[i] = cancel
		go func() {
			r, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/", nil)
			r.Header.Set("Authorization", fmt.Sprintf("Bearer %s%064x", MarkerPrefix, i))
			response, err := f.client.Do(r)
			if err == nil {
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode != 200 {
					err = fmt.Errorf("unexpected HTTP%d", response.StatusCode)
				}
			}
			done <- err
		}()
	}
	defer func() {
		for _, c := range cancels {
			if c != nil {
				c()
			}
		}
	}()
	// Stage requests through admission so the test measures the host Resolve
	// queue, not the client transport's startup SETTINGS/connection races.
	// All requests remain concurrently outstanding when saturation is reached.
	for i := 0; i < cap(f.proxy.requests); i++ {
		start(i)
		select {
		case <-entered:
		case err := <-done:
			t.Fatalf("request completed before Resolve: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("Resolve did not saturate")
		}
	}
	for i := cap(f.proxy.requests); i < maxProtectedRequests; i++ {
		start(i)
		awaitResource(t, func() bool { return len(f.proxy.activeRequests) == i+1 })
	}
	cancels[maxProtectedRequests-1]()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("queued request ignored cancellation")
	}
	awaitResource(t, func() bool { return len(f.proxy.activeRequests) == maxProtectedRequests-1 })
	if active.Load() != int32(cap(f.proxy.requests)) {
		t.Fatal("queue bypassed Resolve bound")
	}
	// One running cancellation releases Resolve for a queued sibling.
	cancels[0]()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled Resolve did not release slot")
	}
	closed := make(chan struct{})
	go func() { f.proxy.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("fence did not cancel saturated Resolve and queue")
	}
	if active.Load() != 0 || len(f.proxy.requests) != 0 || len(f.proxy.activeRequests) != 0 || peak.Load() > int32(cap(f.proxy.requests)) {
		t.Fatal("fence retained/exceeded Resolve work")
	}
	for range maxProtectedRequests - 1 {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("client retained after fence")
		}
	}
}
