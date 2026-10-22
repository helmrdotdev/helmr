package slack

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/platformhttp"
)

func lifecycleSupervisor(t *testing.T, store *CredentialStore, app string) (*SocketSupervisor, <-chan *websocket.Conn) {
	t.Helper()
	sockets := make(chan *websocket.Conn, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/apps.connections.open" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "url": "wss://wss-primary.slack.com/socket"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		hello, _ := json.Marshal(map[string]any{"type": "hello", "connection_info": map[string]string{"app_id": app}})
		if conn.Write(r.Context(), websocket.MessageText, hello) != nil {
			return
		}
		select {
		case sockets <- conn:
		case <-r.Context().Done():
			return
		}
		_, _, _ = conn.Read(r.Context()) // Also answers client pings.
	}))
	t.Cleanup(server.Close)
	transport := platformhttp.Direct()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // Local fake Slack.
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	origin, _ := url.Parse("https://helmr.internal")
	return &SocketSupervisor{cfg: SocketConfig{Credentials: store, AppOrigin: origin, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, http: &http.Client{Transport: transport}}, sockets
}

func TestSocketSupervisorReconnectsAndReleasesOnShutdown(t *testing.T) {
	f, store, _ := socketFixture(t)
	var app string
	if err := f.Pool.QueryRow(t.Context(), `SELECT app_id FROM slack_app_registrations WHERE id=$1`, f.registration).Scan(&app); err != nil {
		t.Fatal(err)
	}
	supervisor, sockets := lifecycleSupervisor(t, store, app)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.Run(ctx) }()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	var first *websocket.Conn
	select {
	case first = <-sockets:
	case <-deadline.C:
		t.Fatal("initial socket absent")
	}
	if err := first.Write(t.Context(), websocket.MessageText, []byte(`{"type":"disconnect","reason":"refresh_requested"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sockets:
	case <-deadline.C:
		t.Fatal("supervisor did not reconnect")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not join connection shutdown")
	}
	var active bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT claim_id IS NOT NULL FROM slack_socket_connections WHERE app_registration_id=$1`, f.registration).Scan(&active); err != nil || active {
		t.Fatalf("shutdown retained lease: %v %v", active, err)
	}
}

func TestSocketRenewalLossClosesLiveConnection(t *testing.T) {
	f, store, _ := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	supervisor, sockets := lifecycleSupervisor(t, store, claim.AppID)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- supervisor.runConnection(ctx, *claim) }()
	select {
	case <-sockets:
	case <-ctx.Done():
		t.Fatal("socket absent")
	}
	// Wait until initial observation, then expire the live claim without sending
	// another input: the renewal loop itself must stop the connection.
	for {
		var connected bool
		if err := f.Pool.QueryRow(ctx, `SELECT status='connected' FROM slack_socket_connections WHERE app_registration_id=$1`, f.registration).Scan(&connected); err != nil {
			t.Fatal(err)
		}
		if connected {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE slack_socket_connections SET expires_at=clock_timestamp()-interval '1 second' WHERE app_registration_id=$1`, f.registration)
	select {
	case err := <-done:
		if !errors.Is(err, errSocketClaimLost) {
			t.Fatalf("renewal loss: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("lost renewal did not close the socket")
	}
}
