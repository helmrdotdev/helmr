package guestd

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHealthResponseDoesNotWaitForLogging(t *testing.T) {
	for _, ready := range []bool{true, false} {
		status := "starting"
		if ready {
			status = "ok"
		}
		t.Run(status, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			release := make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveHealth(listener, func() bool { return ready }, slog.New(blockedHealthLogHandler{release}))
			}()
			t.Cleanup(func() {
				close(release)
				_ = listener.Close()
				<-done
			})
			transport := &http.Transport{}
			client := &http.Client{Transport: transport, Timeout: time.Second}
			t.Cleanup(client.CloseIdleConnections)
			response, err := client.Get("http://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatalf("read complete health response: %v", err)
			}
			want := `{"status":"` + status + `","component":"guestd"}`
			if response.StatusCode != http.StatusOK || string(body) != want {
				t.Fatalf("health response: status=%d body=%q, want 200 %q", response.StatusCode, body, want)
			}
		})
	}
}

type blockedHealthLogHandler struct{ release <-chan struct{} }

func (blockedHealthLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h blockedHealthLogHandler) Handle(context.Context, slog.Record) error {
	<-h.release
	return nil
}
func (h blockedHealthLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h blockedHealthLogHandler) WithGroup(string) slog.Handler      { return h }
