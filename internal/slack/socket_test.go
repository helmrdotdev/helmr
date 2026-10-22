package slack

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/helmrdotdev/helmr/internal/platformhttp"
)

func TestSocketEndpointValidation(t *testing.T) {
	for _, endpoint := range []string{"wss://wss.slack.com/link?ticket=secret", "wss://wss-primary.slack.com/", "wss://wss-backup.slack.com:443/"} {
		if !socketURLAllowed(endpoint) {
			t.Fatalf("rejected Slack endpoint: %s", endpoint)
		}
	}
	for _, endpoint := range []string{"ws://wss.slack.com/", "wss://wss.slack.com.attacker.test/", "wss://user@wss.slack.com/", "wss://wss.slack.com:8443/", "wss://127.0.0.1/", "wss://wss.slack.com/#secret"} {
		if socketURLAllowed(endpoint) {
			t.Fatalf("allowed unexpected endpoint: %s", endpoint)
		}
	}
}

func TestSocketOpenBindsAuthenticatedApp(t *testing.T) {
	for _, app := range []string{"expected", "wrong"} {
		t.Run(app, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/apps.connections.open" {
					if r.Method != "POST" || r.Header.Get("Authorization") != "Bearer xapp-secret" {
						t.Error("missing app authentication")
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "wss://wss-primary.slack.com/link?ticket=secret"})
					return
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("app token forwarded to socket endpoint")
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				raw, _ := json.Marshal(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": app}})
				if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
					return
				}
				_, _, _ = conn.Read(r.Context())
			}))
			defer server.Close()
			transport := platformhttp.Direct()
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local fake Slack certificate.
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()
			supervisor := SocketSupervisor{http: &http.Client{Transport: transport}}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			conn, err := supervisor.openSocket(ctx, socketClaim{Token: "xapp-secret", AppID: "expected"})
			if app == "expected" {
				if err != nil {
					t.Fatal(err)
				}
				conn.CloseNow()
				return
			}
			var failure socketFailure
			if !errors.As(err, &failure) || failure.reason != "app_mismatch" || !failure.actionRequired {
				t.Fatalf("wrong app was not rejected: %v", err)
			}
		})
	}
}

func TestSocketOpenRecognizesExpiredAndInactiveCredentials(t *testing.T) {
	for _, code := range []string{"token_expired", "account_inactive"} {
		t.Run(code, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
			}))
			defer server.Close()
			transport := platformhttp.Direct()
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Test-owned endpoint.
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
			}
			defer transport.CloseIdleConnections()
			supervisor := SocketSupervisor{http: &http.Client{Transport: transport}}
			_, err := supervisor.openSocket(t.Context(), socketClaim{Token: "xapp-expired"})
			var failure socketFailure
			if !errors.As(err, &failure) || !failure.actionRequired || failure.reason != "api_rejected" {
				t.Fatalf("permanent credential failure treated as reconnect: %v", err)
			}
		})
	}
}

func TestSocketOpenUsesConnectProxyAndFailsClosed(t *testing.T) {
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/apps.connections.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "wss://wss-primary.slack.com/socket"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"hello","connection_info":{"app_id":"expected"}}`))
		_, _, _ = conn.Read(r.Context())
	}))
	defer destination.Close()
	var connects, bypasses atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "slack.com:443" && r.Host != "wss-primary.slack.com:443") {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			w.WriteHeader(403)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", destination.Listener.Addr().String())
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
	defer proxy.Close()
	transport, err := platformhttp.External(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local fixture certificate.
	proxyURL, _ := url.Parse(proxy.URL)
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != proxyURL.Host {
			bypasses.Add(1)
			return nil, errors.New("direct network forbidden")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	supervisor := SocketSupervisor{http: &http.Client{Transport: transport}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, err := supervisor.openSocket(ctx, socketClaim{Token: "xapp-test", AppID: "expected"})
	if err != nil {
		t.Fatal(err)
	}
	conn.CloseNow()
	if connects.Load() != 2 || bypasses.Load() != 0 {
		t.Fatal("API and WSS did not both use CONNECT")
	}
	transport.CloseIdleConnections()
	proxy.Close()
	if conn, err := supervisor.openSocket(ctx, socketClaim{Token: "xapp-test", AppID: "expected"}); err == nil {
		conn.CloseNow()
		t.Fatal("proxy outage succeeded")
	}
	if bypasses.Load() != 0 {
		t.Fatal("proxy outage attempted direct access")
	}
}
