package slack

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
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
