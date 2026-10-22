package platformhttp

import (
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExternalConnectAndFailClosed(t *testing.T) {
	var direct, connects atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1); _, _ = w.Write([]byte("ok")) }))
	defer destination.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("expected CONNECT, got %s", r.Method)
			w.WriteHeader(400)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		_, _ = downstream.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() { defer downstream.Close(); defer upstream.Close(); _, _ = io.Copy(upstream, downstream) }()
		_, _ = io.Copy(downstream, upstream)
	}))
	transport, err := External(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local test certificate.
	client := &http.Client{Transport: transport}
	response, err := client.Get(destination.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "ok" || connects.Load() != 1 || direct.Load() != 1 {
		t.Fatal("CONNECT path was not used")
	}
	transport.CloseIdleConnections()
	proxy.Close()
	if response, err := client.Get(destination.URL); err == nil {
		response.Body.Close()
		t.Fatal("unavailable proxy must fail closed")
	}
	if direct.Load() != 1 {
		t.Fatal("request bypassed unavailable proxy")
	}
}

func TestExplicitDirectIgnoresAmbientProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	for _, transport := range []*http.Transport{Direct(), mustExternal(t, "")} {
		if transport.Proxy != nil {
			t.Fatal("direct path inherits ambient proxy")
		}
	}
}

func mustExternal(t *testing.T, raw string) *http.Transport {
	t.Helper()
	transport, err := External(raw)
	if err != nil {
		t.Fatal(err)
	}
	return transport
}

func TestProxyValidationDoesNotExposeCredentials(t *testing.T) {
	for _, raw := range []string{"socks5://secret:password@host", "http://secret:password@host/path", "https://host?q=secret", " http://host", "http://host:70000", "http://"} {
		_, err := External(raw)
		if err == nil {
			t.Errorf("accepted invalid proxy %q", raw)
			continue
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "password") {
			t.Fatal("error exposes credentials")
		}
	}
}
