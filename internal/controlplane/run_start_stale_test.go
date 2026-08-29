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

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

func TestStaleRunStartPreservesPublicSentinelAndFailurePoint(t *testing.T) {
	err := staleAuthority(staleAuthorityRunStart, runStartFailureCheckpointValidation, errStaleRunLeaseClaim)
	if !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatal("typed start failure did not preserve stale Run Lease sentinel")
	}
	point, ok := staleAuthorityPointOf(err)
	if !ok || point != string(runStartFailureCheckpointValidation) {
		t.Fatalf("failure point = %q, %v; want %q, true", point, ok, runStartFailureCheckpointValidation)
	}
	if err.Error() != "run start authority is stale" {
		t.Fatalf("error = %q; want operation-owned stale diagnostic", err)
	}
}

func TestStaleRunStartDoesNotClassifyUnrelatedErrors(t *testing.T) {
	original := errors.New("storage unavailable")
	err := staleAuthority(staleAuthorityRunStart, runStartFailureRuntime, original)
	if !errors.Is(err, original) {
		t.Fatal("unrelated error was replaced")
	}
	if _, ok := staleAuthorityPointOf(err); ok {
		t.Fatal("unrelated error received a start failure point")
	}
}

func TestStaleRunStartKeepsInnermostFailurePoint(t *testing.T) {
	err := staleAuthority(staleAuthorityRunStart, runStartFailureCheckpoint, errStaleRunLeaseClaim)
	err = staleAuthority(staleAuthorityRunStart, runStartFailureRuntime, err)
	point, ok := staleAuthorityPointOf(err)
	if !ok || point != string(runStartFailureCheckpoint) {
		t.Fatalf("failure point = %q, %v; want %q, true", point, ok, runStartFailureCheckpoint)
	}
}

func TestWorkerStartLogsOnlyTypedFailurePointAndKeepsPublicConflict(t *testing.T) {
	worker, _, authority := validRunLeaseClaimFixture()
	const secretSentinel = "https://signed.invalid/object?credential=secret-sentinel"
	store := &staleRunStartStore{
		failure: errors.Join(pgx.ErrNoRows, errors.New(secretSentinel)),
	}
	var logs bytes.Buffer
	server := &Server{
		log: slog.New(slog.NewJSONHandler(&logs, nil)),
		db:  store,
	}
	body, err := json.Marshal(workerapi.RunStartRequest{
		Lease: workerapi.RunLeaseFence{
			ID:            pgvalue.UUIDString(authority.runLease.ID),
			LeaseSequence: authority.runLease.LeaseSequence,
		},
		Fresh: &workerapi.RunStartFresh{},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/worker/v1/run/start", bytes.NewReader(body))
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
		`"details":{"point":"locators"}`,
	} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("public conflict body missing %s: %s", want, response.Body)
		}
	}
	for _, want := range []string{
		`"failure_point":"locators"`,
		`"run_lease_id":"` + pgvalue.UUIDString(authority.runLease.ID) + `"`,
		`"lease_sequence":1`,
		fmt.Sprintf(`"worker_epoch":%d`, worker.WorkerEpoch),
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
	db.Querier
	failure    error
	committed  bool
	rolledBack bool
}

func (s *staleRunStartStore) BeginQuerier(context.Context) (db.Querier, transaction, error) {
	return s, staleRunStartTransaction{store: s}, nil
}

func (s *staleRunStartStore) GetRunLeaseStartLocators(
	context.Context,
	db.GetRunLeaseStartLocatorsParams,
) (db.GetRunLeaseStartLocatorsRow, error) {
	return db.GetRunLeaseStartLocatorsRow{}, s.failure
}

type staleRunStartTransaction struct {
	store *staleRunStartStore
}

func (tx staleRunStartTransaction) Commit(context.Context) error {
	tx.store.committed = true
	return nil
}

func (tx staleRunStartTransaction) Rollback(context.Context) error {
	tx.store.rolledBack = true
	return nil
}
