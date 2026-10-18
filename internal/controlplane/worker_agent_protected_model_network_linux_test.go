//go:build linux && computerproof

package controlplane

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

func TestProtectedModelNetwork(t *testing.T) {
	if os.Getenv("HELMR_TEST_NAMESPACE") != "1" {
		t.Skip("requires disposable privileged namespace fixture")
	}
	t.Setenv("HELMR_NATIVE_EXECUTION_PROOF", "1")
	f := startProtectedModel(t, t.TempDir())
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, address := range addresses {
		if strings.HasPrefix(address.String(), protectedModelIP+"/") {
			t.Fatal("upstream is a host address")
		}
	}
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", protectedModelIP+":53")
	}}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ips, err := resolver.LookupHost(ctx, "protected.fixture")
	if err != nil || len(ips) != 1 || ips[0] != protectedModelIP {
		t.Fatalf("fixture DNS: %v %v", ips, err)
	}
	if _, err := resolver.LookupHost(ctx, "denied.example"); err == nil {
		t.Fatal("unexpected external DNS result")
	}
	roots := x509.NewCertPool()
	pem, err := os.ReadFile(f.CA)
	if err != nil {
		t.Fatal(err)
	}
	roots.AppendCertsFromPEM(pem)
	dialer := &net.Dialer{Resolver: resolver}
	transport := &http.Transport{DialContext: dialer.DialContext, TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(ctx, "POST", f.Origin+"/pi-anthropic/http/v1/messages", strings.NewReader(`{"messages":[]}`))
	req.Header.Set("X-Api-Key", protectedModelToken)
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 || !strings.Contains(string(body), "protected fixture reply") {
		t.Fatalf("SSE: %d %s %v", resp.StatusCode, body, err)
	}
	config, err := websocket.NewConfig("wss"+strings.TrimPrefix(f.Origin, "https")+"/codex/ws/v1/responses", f.Origin)
	if err != nil {
		t.Fatal(err)
	}
	config.TlsConfig = &tls.Config{RootCAs: roots, ServerName: "protected.fixture", MinVersion: tls.VersionTLS12}
	config.Header.Set("Authorization", "Bearer "+protectedModelToken)
	// The TLS dial remains numeric, as it does in the protected proxy.
	_, port, _ := net.SplitHostPort(config.Location.Host)
	connection, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(protectedModelIP, port), config.TlsConfig)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := websocket.NewClient(config, connection)
	if err != nil {
		connection.Close()
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetDeadline(time.Now().Add(5 * time.Second))
	if err := websocket.Message.Send(ws, `{"type":"response.create","input":[]}`); err != nil {
		t.Fatal(err)
	}
	for {
		var event string
		if err := websocket.Message.Receive(ws, &event); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(event, "response.completed") {
			break
		}
	}
	ws.Close()
}
