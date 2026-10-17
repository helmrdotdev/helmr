package workerclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWatchUpdatesAuthenticatesWakesAndJoinsCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/instance/credential" {
			_ = json.NewEncoder(w).Encode(workerapi.HostCredentialResponse{WorkerEpoch: 1, Credential: "secret", ExpiresInSeconds: 3600})
			return
		}
		if r.URL.Path != "/worker/v1/updates" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("unexpected update request: %s", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": heartbeat\n\nevent: change\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("00000000-0000-0000-0000-000000000401", "worker-secret"), WithService("00000000-0000-0000-0000-000000000901"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wake := make(chan struct{}, 2)
	done := make(chan error, 1)
	go func() { done <- client.WatchUpdates(ctx, func() { wake <- struct{}{} }) }()
	select {
	case <-wake:
	case <-time.After(3 * time.Second):
		t.Fatal("update did not wake")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel returned nil")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not join")
	}
	select {
	case <-wake:
		t.Fatal("heartbeat caused extra wake")
	default:
	}
}
