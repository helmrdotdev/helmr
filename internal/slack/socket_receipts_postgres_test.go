package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/coder/websocket"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func socketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- conn
		<-done
		conn.CloseNow()
	}))
	client, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		close(done)
		server.Close()
		t.Fatal(err)
	}
	peer := <-accepted
	t.Cleanup(func() { client.CloseNow(); close(done); server.Close() })
	return client, peer
}

func TestSocketReceiptsCommitBeforeACKAndDeduplicate(t *testing.T) {
	f, store, _ := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://helmr.internal")
	key := bytes.Repeat([]byte("k"), 32)
	supervisor := SocketSupervisor{cfg: SocketConfig{Credentials: store, AppOrigin: origin, ControlKey: key, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	client, peer := socketPair(t)
	event := eventBody(t, "socket-event", map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> work"})
	control := f.control("choose")
	control.Target.Turn, control.Target.Ask = f.pendingQuestion(t)
	control.OptionID = "yes"
	form, _ := url.ParseQuery(string(interactionBody(t, InteractionHandler{ControlKey: key}, control, "human", slackNow())))
	lifecycle := f.control("help")
	lifecycle.ExpiresAt = 0
	lifecycleForm, _ := url.ParseQuery(string(interactionBody(t, InteractionHandler{ControlKey: key}, lifecycle, "human", slackNow())))
	for index, input := range []struct {
		kind    string
		payload []byte
	}{{"events_api", event}, {"interactive", []byte(form.Get("payload"))}, {"interactive", []byte(lifecycleForm.Get("payload"))}} {
		for range 2 {
			envelope := socketEnvelope{Type: input.kind, EnvelopeID: uuid.NewV7().String(), AcceptsResponse: true, Payload: input.payload}
			raw, _ := json.Marshal(envelope)
			if err := supervisor.receiveSocketEnvelope(t.Context(), client, *claim, raw); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			_, raw, err = peer.Read(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			var ack struct {
				ID string `json:"envelope_id"`
			}
			if json.Unmarshal(raw, &ack) != nil || ack.ID != envelope.EnvelopeID {
				t.Fatalf("wrong ACK: %s", raw)
			}
			var count int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests WHERE status='received'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			expected := index + 1
			if count != expected {
				t.Fatalf("ACK preceded durable deduplicated receipt: %d", count)
			}
		}
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_controls`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("receipt synchronously changed core: %d %v", count, err)
	}
}

func TestSocketTransientReceiptFailureDoesNotACK(t *testing.T) {
	f, store, _ := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_socket_receipt() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected receipt failure'; END $$; CREATE TRIGGER reject_socket_receipt BEFORE INSERT ON slack_requests FOR EACH ROW EXECUTE FUNCTION reject_socket_receipt()`)
	client, peer := socketPair(t)
	supervisor := SocketSupervisor{cfg: SocketConfig{Credentials: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	raw, _ := json.Marshal(socketEnvelope{Type: "events_api", EnvelopeID: "retry", Payload: eventBody(t, "retry-event", map[string]any{"type": "app_mention", "user": "human", "channel": "C1", "ts": slackNow(), "text": "<@bot> work"})})
	if err := supervisor.receiveSocketEnvelope(t.Context(), client, *claim, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, raw, err := peer.Read(ctx); err == nil {
		t.Fatalf("transient failure ACKed: %s", raw)
	}
	var count int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM slack_requests`).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed receipt persisted", err)
	}
}

func TestSocketDiscardLogsOnlyRedactedReason(t *testing.T) {
	_, store, _ := socketFixture(t)
	claim, err := store.acquireSocket(t.Context(), uuid.NewV7())
	if err != nil || claim == nil {
		t.Fatal(err)
	}
	client, peer := socketPair(t)
	var logs bytes.Buffer
	supervisor := SocketSupervisor{cfg: SocketConfig{Credentials: store, Log: slog.New(slog.NewTextHandler(&logs, nil))}}
	raw, _ := json.Marshal(socketEnvelope{Type: "unknown", EnvelopeID: "discard", Payload: json.RawMessage(`{"secret":"payload-sentinel"}`)})
	if err := supervisor.receiveSocketEnvelope(t.Context(), client, *claim, raw); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, _, err := peer.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "reason=invalid") || strings.Contains(logs.String(), "payload-sentinel") {
		t.Fatalf("discard log lost its redacted reason: %s", logs.String())
	}
}
