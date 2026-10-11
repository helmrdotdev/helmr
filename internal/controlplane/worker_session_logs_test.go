package controlplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerSessionLogDurableReceiptAndFences(t *testing.T) {
	f := agenttest.New(t)
	handler := newPostgresServer(t, f.Pool)
	var lose atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/sessions/logs" && lose.CompareAndSwap(true, false) {
			committed := httptest.NewRecorder()
			handler.ServeHTTP(committed, r)
			if committed.Code != http.StatusOK {
				t.Errorf("commit status %d", committed.Code)
			}
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	session := runtimeTestSession(f)
	attachment, err := client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	request := workerapi.SessionLogRequest{Session: session, AttachmentSequence: attachment.AttachmentSequence, Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte{0, 255, 128}}
	lose.Store(true)
	_, _ = client.AppendSessionLog(t.Context(), request) // The server may commit before the receipt disappears.
	client = auth.client(t, server.URL)
	receipt, err := client.AppendSessionLog(t.Context(), request)
	if err != nil || receipt.ThroughSequence != 1 || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour {
		t.Fatalf("durable retry %+v %v", receipt, err)
	}
	var count int
	var data []byte
	var accepted time.Time
	if err = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM telemetry_outbox),data,accepted_at FROM telemetry_outbox WHERE sequence=1`).Scan(&count, &data, &accepted); err != nil || count != 1 || !bytes.Equal(data, request.Data) || !accepted.Equal(receipt.AcceptedAt) {
		t.Fatalf("durable identity count=%d data=%v time=%v err=%v", count, data, accepted, err)
	}
	changed := request
	changed.Data = []byte{0, 255, 127}
	if _, err = client.AppendSessionLog(t.Context(), changed); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("changed retry %v", err)
	}
	attachment, err = client.AcquireAgentAttachment(t.Context(), session)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.AppendSessionLog(t.Context(), request); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("stale attachment %v", err)
	}
	request.AttachmentSequence = attachment.AttachmentSequence
	replay, err := client.AppendSessionLog(t.Context(), request)
	if err != nil || !replay.AcceptedAt.Equal(receipt.AcceptedAt) {
		t.Fatalf("new attachment retry %+v %v", replay, err)
	}
	changed = request
	changed.Session.ComputerLeaseEpoch++
	if _, err = client.AppendSessionLog(t.Context(), changed); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("foreign physical owner %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE session_processes SET stdout_last_accepted_at=stdout_last_accepted_at-interval '2200 hours',stdout_last_expires_at=stdout_last_expires_at-interval '2200 hours'`)
	expired, err := client.AppendSessionLog(t.Context(), request)
	if err != nil || !expired.Expired || expired.ThroughSequence != request.ThroughSequence {
		t.Fatalf("expired disposition %+v %v", expired, err)
	}
}
