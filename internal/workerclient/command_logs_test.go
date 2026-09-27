package workerclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCommandLogRefreshPreservesChunkIdentityAndBytes(t *testing.T) {
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/worker/v1/instance/token":
			_ = json.NewEncoder(w).Encode(workerapi.TokenResponse{Token: "token", ExpiresInSeconds: 3600})
		case "/worker/v1/run/computer-commands/logs/append":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			bodies = append(bodies, body)
			if len(bodies) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, WithHTTPClient(server.Client()), WithAuth("worker", "secret"), WithService("service"))
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.CommandLogAppendRequest{
		OrgID: "019c0225-f0c9-7f66-8a23-7782ca0a8460", CommandID: "019c0225-f0c9-7f66-8a23-7782ca0a8461",
		ComputerInstanceID: "019c0225-f0c9-7f66-8a23-7782ca0a8462", WriterGeneration: 9,
		Stream: workerapi.LogStreamStderr, ObservedSeq: 42, ObservedAt: time.Now().UTC().Truncate(time.Millisecond),
		Content: []byte{0, 255, 128},
	}
	if err := client.AppendCommandLog(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("token refresh changed log delivery: %q", bodies)
	}
	var decoded workerapi.CommandLogAppendRequest
	if err := json.Unmarshal(bodies[1], &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CommandID != request.CommandID || decoded.ComputerInstanceID != request.ComputerInstanceID ||
		decoded.WriterGeneration != request.WriterGeneration || decoded.ObservedSeq != request.ObservedSeq ||
		!decoded.ObservedAt.Equal(request.ObservedAt) || !bytes.Equal(decoded.Content, request.Content) {
		t.Fatalf("chunk changed: %+v", decoded)
	}
}
