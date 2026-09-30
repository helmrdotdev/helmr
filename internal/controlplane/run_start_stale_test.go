package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestStaleRunStartPreservesPublicSentinelAndFailurePoint(t *testing.T) {
	err := staleAuthority(staleAuthorityRunStart, "execution", errStaleRunLeaseClaim)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatal("typed start failure did not preserve stale Run Lease sentinel")
	}
	point, ok := staleAuthorityPointOf(err)
	if !ok || point != string("execution") {
		t.Fatalf("failure point = %q, %v; want %q, true", point, ok, "execution")
	}
	if err.Error() != "run start authority is stale" {
		t.Fatalf("error = %q; want operation-owned stale diagnostic", err)
	}
}

func TestStaleRunStartDoesNotClassifyUnrelatedErrors(t *testing.T) {
	original := errors.New("storage unavailable")
	err := staleAuthority(staleAuthorityRunStart, "outer", original)
	if !errors.Is(err, original) {
		t.Fatal("unrelated error was replaced")
	}
	if _, ok := staleAuthorityPointOf(err); ok {
		t.Fatal("unrelated error received a start failure point")
	}
}

func TestStaleRunStartKeepsInnermostFailurePoint(t *testing.T) {
	err := staleAuthority(staleAuthorityRunStart, "inner", errStaleRunLeaseClaim)
	err = staleAuthority(staleAuthorityRunStart, "outer", err)
	point, ok := staleAuthorityPointOf(err)
	if !ok || point != string("inner") {
		t.Fatalf("failure point = %q, %v; want %q, true", point, ok, "inner")
	}
}

func TestWorkerStartLogsOnlyTypedFailurePointAndKeepsPublicConflict(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "starting", time.Now())
	worker := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1, HostClaimVersion: 1, GroupClaimVersion: 1}
	const secretSentinel = "https://signed.invalid/object?credential=secret-sentinel"
	store := &staleRunStartStore{
		pool:    f.Pool,
		failure: errors.Join(pgx.ErrNoRows, errors.New(secretSentinel)),
	}
	var logs bytes.Buffer
	server := &Server{
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
		db:  db.New(f.Pool),
		tx:  store,
	}
	body, err := json.Marshal(workerapi.RunStartRequest{
		Lease: workerapi.RunLeaseFence{
			ID:            pgvalue.UUIDString(pgvalue.UUID(work.LeaseID)),
			LeaseSequence: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := newWorkerRequest(http.MethodPost, "/worker/v1/run/start", bytes.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), workerContextKey{}, worker))
	response := httptest.NewRecorder()

	server.workerStart(response, request)

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
		fmt.Sprintf(`"worker_epoch":%d`, worker.Epoch),
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("structured log missing %s: %s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), `"error"`) {
		t.Fatalf("structured log included an underlying error: %s", logs.String())
	}
}

type staleRunStartStore struct {
	pool                  *pgxpool.Pool
	failure               error
	committed, rolledBack bool
}

func (s *staleRunStartStore) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.Begin(ctx)
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
