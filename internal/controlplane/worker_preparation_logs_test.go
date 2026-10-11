package controlplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/httpclient"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestWorkerPreparationLogDurableReceiptAndFences(t *testing.T) {
	f, executor := preparationTransportFixture(t)
	handler := newPostgresServer(t, f.Pool)
	var lose atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/worker/v1/allocations/preparation/logs" && lose.CompareAndSwap(true, false) {
			committed := httptest.NewRecorder()
			handler.ServeHTTP(committed, r)
			if committed.Code != http.StatusOK {
				t.Errorf("commit status %d", committed.Code)
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	auth := seedHostSecret(t, f.Pool, f.Worker)
	client := auth.client(t, server.URL)
	request := workerapi.PreparationLogRequest{Executor: executor, Stream: "stdout", Kind: "data", Sequence: 1, ThroughSequence: 1, ObservedAtUnixNano: time.Now().UnixNano(), Data: []byte{0, 255, 128}}
	lose.Store(true)
	_, _ = client.AppendPreparationLog(t.Context(), request)
	client = auth.client(t, server.URL)
	receipt, err := client.AppendPreparationLog(t.Context(), request)
	if err != nil || receipt.ThroughSequence != 1 || receipt.ExpiresAt.Sub(receipt.AcceptedAt) != 90*24*time.Hour {
		t.Fatalf("receipt: %v %v", receipt, err)
	}
	var count int
	var data []byte
	var accepted time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM telemetry_outbox),data,accepted_at FROM telemetry_outbox WHERE sequence=1`).Scan(&count, &data, &accepted); err != nil || count != 1 || !bytes.Equal(data, request.Data) || !accepted.Equal(receipt.AcceptedAt) {
		t.Fatalf("durable retry count=%d bytes=%v accepted=%v err=%v", count, data, accepted, err)
	}
	changed := request
	changed.Data = []byte("x")
	if _, err := client.AppendPreparationLog(t.Context(), changed); !httpclient.IsStatus(err, http.StatusConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	changed = request
	changed.Executor.Identity.Epoch++
	if _, err := client.AppendPreparationLog(t.Context(), changed); err == nil {
		t.Fatal("foreign epoch accepted")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET stdout_last_accepted_at=stdout_last_accepted_at-interval '2200 hours',stdout_last_expires_at=stdout_last_expires_at-interval '2200 hours'`)
	expired, err := client.AppendPreparationLog(t.Context(), request)
	if err != nil || !expired.Expired || expired.ThroughSequence != 1 {
		t.Fatalf("expired receipt: %v %v", expired, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_preparations SET executor_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, executor.Identity.OwnerID)
	if _, err := client.AppendPreparationLog(t.Context(), request); err == nil {
		t.Fatal("expired executor accepted")
	}
}
