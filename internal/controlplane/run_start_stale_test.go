package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestWorkerStartLogsOnlyTypedFailurePointAndKeepsPublicConflict(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "starting", time.Now())
	const secretSentinel = "https://signed.invalid/object?credential=secret-sentinel"
	store := &staleRunStartStore{
		Pool:    f.Pool,
		failure: errors.Join(pgx.ErrNoRows, errors.New(secretSentinel)),
	}
	var logs bytes.Buffer
	handler := newPostgresServer(t, f.Pool, func(cfg *ServerConfig) {
		cfg.Log = slog.New(slog.NewJSONHandler(&logs, nil))
		cfg.TX = store
	})
	worker := newWorkerHTTPClient(t, handler, f.Pool, f.WorkerID)
	const epoch = 1

	response := worker.send(t, "/worker/v1/run/leases/start", workerapi.RunStartRequest{
		Lease: workerapi.RunLeaseFence{
			ID:            pgvalue.UUIDString(pgvalue.UUID(work.LeaseID)),
			LeaseSequence: 1,
		},
	})

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusConflict, response.Body)
	}
	if !store.rolledBack || store.committed {
		t.Fatalf("transaction lifecycle = committed:%v rolled_back:%v; want rollback only", store.committed, store.rolledBack)
	}
	for name, text := range map[string]string{"response": response.Body.String(), "logs": logs.String()} {
		if strings.Contains(text, secretSentinel) {
			t.Fatalf("%s leaked underlying error sentinel", name)
		}
	}
	for _, want := range []string{
		`"code":"run_start_stale"`,
		`"message":"run start authority is stale"`,
		`"details":{"point":"execution"}`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("public conflict body missing %s: %s", want, response.Body)
		}
	}
	for _, want := range []string{
		`"failure_point":"execution"`,
		`"run_lease_id":"` + pgvalue.UUIDString(pgvalue.UUID(work.LeaseID)) + `"`,
		`"lease_sequence":1`,
		fmt.Sprintf(`"worker_epoch":%d`, epoch),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("structured log missing %s: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), `"error"`) {
		t.Fatalf("structured log included an underlying error: %s", logs.String())
	}
}

// staleRunStartStore serves the pool, except that every query in a
// transaction it begins fails.
type staleRunStartStore struct {
	*pgxpool.Pool
	failure               error
	committed, rolledBack bool
}

func (s *staleRunStartStore) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return staleRunStartTransaction{Tx: tx, store: s}, nil
}

type staleRunStartTransaction struct {
	pgx.Tx
	store *staleRunStartStore
}

func (tx staleRunStartTransaction) QueryRow(context.Context, string, ...any) pgx.Row {
	return staleRunStartRow{err: tx.store.failure}
}
func (tx staleRunStartTransaction) Commit(ctx context.Context) error {
	tx.store.committed = true
	return tx.Tx.Commit(ctx)
}
func (tx staleRunStartTransaction) Rollback(ctx context.Context) error {
	tx.store.rolledBack = true
	return tx.Tx.Rollback(ctx)
}

type staleRunStartRow struct{ err error }

func (row staleRunStartRow) Scan(...any) error { return row.err }
