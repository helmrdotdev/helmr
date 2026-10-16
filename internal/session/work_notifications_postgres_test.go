package session_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func TestOutputOnlyWakesReadersWhileNewWorkWakesWorker(t *testing.T) {
	f := newFixture(t)
	running := f.enqueue(t, "running")
	if _, err := dispatchTurnForTest(t.Context(), f.pool, f.execution()); err != nil {
		t.Fatal(err)
	}
	if err := session.RuntimeRegisterMessages(t.Context(), f.pool, *f.host(), f.execution(), running.TurnID); err != nil {
		t.Fatal(err)
	}
	listener, err := pgx.ConnectConfig(t.Context(), f.pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close(context.Background())
	if _, err = listener.Exec(t.Context(), "LISTEN helmr_changes"); err != nil {
		t.Fatal(err)
	}
	topics := func() map[string]bool {
		t.Helper()
		marker := uuid.NewV7().String()
		if _, err := f.pool.Exec(t.Context(), `SELECT pg_notify('helmr_changes',$1)`, marker); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		result := map[string]bool{}
		for {
			notification, err := listener.WaitForNotification(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if notification.Payload == marker {
				return result
			}
			result[notification.Payload] = true
		}
	}
	for range 3 {
		if _, err := session.RuntimeOutput(t.Context(), f.pool, *f.host(), f.execution(), running.TurnID, uuid.NewV7(), json.RawMessage(`[{"type":"text","text":"progress"}]`)); err != nil {
			t.Fatal(err)
		}
	}
	observed := topics()
	if !observed["session:"+f.session.String()] || !observed["slack_projection"] || observed["worker:"+f.worker.String()] || observed["dispatch"] {
		t.Fatalf("output notification targets: %v", observed)
	}
	f.enqueue(t, "successor")
	if observed = topics(); !observed["worker:"+f.worker.String()] || !observed["dispatch"] {
		t.Fatalf("queued Turn did not wake execution: %v", observed)
	}
	_, err = session.SendTurn(t.Context(), f.pool, f.caller(), session.EnqueueRequest{EnvironmentID: f.env, SessionID: f.session, IdempotencyKey: "steer", Input: json.RawMessage(`[{"type":"text","text":"steer"}]`)}, running.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if observed = topics(); !observed["worker:"+f.worker.String()] {
		t.Fatalf("steering message did not wake worker: %v", observed)
	}
}
