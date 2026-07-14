package control

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestWorkerLeaseRejectsPlacementCapabilities(t *testing.T) {
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/lease", strings.NewReader(`{"capabilities":{"supports_run":true}}`))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(uuid.Must(uuid.NewV7()))))
	rec := httptest.NewRecorder()

	server.workerLease(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerStartRejectsLeaseFromAnotherWorkerEpoch(t *testing.T) {
	leaseWorkerID := uuid.Must(uuid.NewV7())
	requestWorkerID := uuid.Must(uuid.NewV7())
	request := api.WorkerStartRequest{Lease: finalWorkerRunLease(leaseWorkerID)}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(requestWorkerID)))
	rec := httptest.NewRecorder()

	server.workerStart(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseRejectsUnknownFields(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	body, err := json.Marshal(map[string]any{
		"lease":             lease,
		"result":            map[string]any{"kind": "completed", "active_duration_ms": 1},
		"unknown_authority": "removed-authority",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: &workerContractStore{}}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerRelease(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestWorkerReleaseRetriesCanonicalTerminalResponseAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	result := api.WorkerReleaseResult{Kind: "completed", ActiveDurationMs: 42}
	fingerprint, err := terminalRequestFingerprint("run.release", result)
	if err != nil {
		t.Fatal(err)
	}
	store := &workerResponseLossStore{runTerminal: db.GetRunLeaseTerminalResultRow{
		RunStatus:                  db.RunStatusSucceeded,
		TerminalRequestFingerprint: pgtype.Text{String: fingerprint, Valid: true},
	}}
	requestBody, err := json.Marshal(api.WorkerReleaseRequest{Lease: lease, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(requestBody))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerRelease(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"succeeded"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.releaseCalls != 0 {
		t.Fatalf("release side effects repeated %d times", store.releaseCalls)
	}
	store.runTerminal.TerminalRequestFingerprint.String = "sha256:different-terminal-payload"
	req = httptest.NewRequest(http.MethodPost, "/api/worker/leases/release", bytes.NewReader(requestBody))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec = httptest.NewRecorder()
	server.workerRelease(rec, req)
	if rec.Code != http.StatusConflict || store.releaseCalls != 0 {
		t.Fatalf("different terminal payload status=%d side_effects=%d body=%s", rec.Code, store.releaseCalls, rec.Body.String())
	}
}

func TestWorkerStartRetriesCanonicalStartedResponseAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerRunLease(workerID)
	expiresAt := time.Now().Add(5 * time.Minute).UTC()
	store := &workerResponseLossStore{startedRun: db.RunLease{
		State: db.RunLeaseStateRunning, ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}}
	body, err := json.Marshal(api.WorkerStartRequest{Lease: lease})
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{log: discardTestLogger(), db: store}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/leases/start", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerStart(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"running"`) {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.startCalls != 0 {
		t.Fatalf("start mutation repeated %d times", store.startCalls)
	}
}

func TestWorkerBuildTerminalRetriesAfterResponseLoss(t *testing.T) {
	workerID := uuid.Must(uuid.NewV7())
	lease := finalWorkerBuildLease(workerID)
	lease.ExpiresAt = time.Now().Add(-time.Minute)
	result := api.WorkerDeploymentBuildResult{Error: new("deterministic build failure")}
	completeFingerprint, err := terminalRequestFingerprint("deployment_build.complete", result)
	if err != nil {
		t.Fatal(err)
	}
	store := &workerResponseLossStore{buildTerminal: db.GetDeploymentBuildTerminalResultRow{
		State:                      db.DeploymentBuildLeaseStateFailed,
		TerminalRequestFingerprint: pgtype.Text{String: completeFingerprint, Valid: true},
	}}
	server := &Server{log: discardTestLogger(), db: store}
	body, err := json.Marshal(api.WorkerCompleteDeploymentBuildRequest{Lease: lease, Result: result})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/deployment-builds/complete", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec := httptest.NewRecorder()

	server.workerCompleteDeploymentBuild(rec, req)

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"failed"`) {
		t.Fatalf("completion retry status = %d body=%s", rec.Code, rec.Body.String())
	}
	if store.completeCalls != 0 || store.failCalls != 0 {
		t.Fatalf("build side effects repeated: complete=%d fail=%d", store.completeCalls, store.failCalls)
	}

	reason := "insufficient_capacity"
	rejectFingerprint, err := terminalRequestFingerprint("deployment_build.reject", struct {
		ReasonCode string          `json:"reason_code"`
		Error      json.RawMessage `json:"error,omitempty"`
	}{ReasonCode: reason})
	if err != nil {
		t.Fatal(err)
	}
	store.buildTerminal = db.GetDeploymentBuildTerminalResultRow{
		State:                      db.DeploymentBuildLeaseStateRejected,
		TerminalRequestFingerprint: pgtype.Text{String: rejectFingerprint, Valid: true},
	}
	body, err = json.Marshal(api.WorkerDeploymentBuildRejectRequest{Lease: lease, ReasonCode: reason})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodPost, "/api/worker/deployment-builds/reject", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, finalWorkerActor(workerID)))
	rec = httptest.NewRecorder()

	server.workerRejectDeploymentBuild(rec, req)

	if rec.Code != http.StatusNoContent || store.rejectCalls != 0 {
		t.Fatalf("reject retry status=%d side_effects=%d body=%s", rec.Code, store.rejectCalls, rec.Body.String())
	}
}

func finalWorkerRunLease(workerID uuid.UUID) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(), RunID: uuid.Must(uuid.NewV7()).String(),
		WorkerGroupID: "group-1", WorkerInstanceID: workerID.String(), WorkerEpoch: 2, LeaseSequence: 3, SnapshotVersion: 4,
		RuntimeInstanceID: uuid.Must(uuid.NewV7()).String(), NetworkSlotID: uuid.Must(uuid.NewV7()).String(), NetworkSlotGeneration: 5,
		ProtocolVersion: api.CurrentWorkerProtocolVersion, AttemptNumber: 1,
	}
}

func finalWorkerBuildLease(workerID uuid.UUID) api.WorkerDeploymentBuildLease {
	return api.WorkerDeploymentBuildLease{
		ID: uuid.Must(uuid.NewV7()).String(), OrgID: uuid.Must(uuid.NewV7()).String(),
		ProjectID: uuid.Must(uuid.NewV7()).String(), EnvironmentID: uuid.Must(uuid.NewV7()).String(),
		DeploymentID: uuid.Must(uuid.NewV7()).String(), WorkerGroupID: "group-1", WorkerInstanceID: workerID.String(),
		WorkerEpoch: 2, BuildAttemptNumber: 1, LeaseSequence: 1, WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		ExpiresAt: time.Now().Add(time.Minute), RequestedWorkloadDiskBytes: 1, RequestedScratchBytes: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedBuildExecutors: 1,
	}
}

func finalWorkerActor(workerID uuid.UUID) workerActor {
	return workerActor{WorkerInstanceID: workerID, WorkerGroupID: "group-1", WorkerEpoch: 2, ProtocolVersion: api.CurrentWorkerProtocolVersion}
}

func discardTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type workerContractStore struct{ db.Querier }

type workerResponseLossStore struct {
	db.Querier
	runTerminal   db.GetRunLeaseTerminalResultRow
	startedRun    db.RunLease
	buildTerminal db.GetDeploymentBuildTerminalResultRow
	startCalls    int
	releaseCalls  int
	completeCalls int
	failCalls     int
	rejectCalls   int
}

func (s *workerResponseLossStore) GetRunLeaseTerminalResult(context.Context, db.GetRunLeaseTerminalResultParams) (db.GetRunLeaseTerminalResultRow, error) {
	return s.runTerminal, nil
}

func (s *workerResponseLossStore) GetStartedRunLease(context.Context, db.GetStartedRunLeaseParams) (db.RunLease, error) {
	return s.startedRun, nil
}

func (s *workerResponseLossStore) StartRunLease(context.Context, db.StartRunLeaseParams) (db.StartRunLeaseRow, error) {
	s.startCalls++
	return db.StartRunLeaseRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) GetDeploymentBuildTerminalResult(context.Context, db.GetDeploymentBuildTerminalResultParams) (db.GetDeploymentBuildTerminalResultRow, error) {
	return s.buildTerminal, nil
}

func (s *workerResponseLossStore) ReleaseRunLease(context.Context, db.ReleaseRunLeaseParams) (db.ReleaseRunLeaseRow, error) {
	s.releaseCalls++
	return db.ReleaseRunLeaseRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) CompleteDeploymentBuild(context.Context, db.CompleteDeploymentBuildParams) (db.CompleteDeploymentBuildRow, error) {
	s.completeCalls++
	return db.CompleteDeploymentBuildRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) FailDeploymentBuild(context.Context, db.FailDeploymentBuildParams) (db.FailDeploymentBuildRow, error) {
	s.failCalls++
	return db.FailDeploymentBuildRow{}, pgx.ErrNoRows
}

func (s *workerResponseLossStore) RejectDeploymentBuildLease(context.Context, db.RejectDeploymentBuildLeaseParams) (db.DeploymentBuildLease, error) {
	s.rejectCalls++
	return db.DeploymentBuildLease{}, pgx.ErrNoRows
}
