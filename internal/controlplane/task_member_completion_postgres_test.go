package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func taskHTTPExecutionFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, run.ExecutionFence, workerapi.CompleteTaskRequest) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := run.ClaimExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = run.StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = run.EnterExecution(t.Context(), tx, fence, a.Run.EntrypointKind, a.Run.EntrypointDeclaredID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	request := workerapi.CompleteTaskRequest{Lease: workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}, OperationID: uuid.NewV7().String(), Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{Output: json.RawMessage(`{"answer":42}`)}}}
	return f, work, fence, request
}

func TestTaskCompletionHTTPPreservesReceiptAndStaleDiagnostics(t *testing.T) {
	f, work, fence, request := taskHTTPExecutionFixture(t)
	var logs bytes.Buffer
	s := &Server{db: db.New(f.Pool), tx: f.Pool, log: slog.New(slog.NewTextHandler(&logs, nil))}
	worker := workerActor{WorkerHostID: f.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: fence.HostClaimVersion, GroupClaimVersion: fence.GroupClaimVersion}
	invoke := func(r workerapi.CompleteTaskRequest) *httptest.ResponseRecorder {
		t.Helper()
		body, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/run/tasks/complete", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, worker))
		out := httptest.NewRecorder()
		s.workerCompleteTask(out, req)
		return out
	}
	out := invoke(request)
	if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), `"code":"task_completion_stale"`) || !strings.Contains(out.Body.String(), `"point":"execution"`) {
		t.Fatalf("running lease response=%d %s", out.Code, out.Body.String())
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = run.BeginExecutionFinalization(t.Context(), tx, run.ExecutionFinalization{Fence: fence, RunID: pgvalue.UUID(work.RunID), AttemptNumber: 1, OperationID: pgvalue.UUID(uuid.MustParse(request.OperationID)), Fingerprint: dbtest.Digest("finalization")}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true}' WHERE id=$1`, work.RunID)
	invalid := request
	invalid.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "failed", Details: json.RawMessage(`{}`)}}
	out = invoke(invalid)
	if out.Code != http.StatusUnprocessableEntity || !strings.Contains(out.Body.String(), `"code":"unprocessable_entity"`) {
		t.Fatalf("invalid pinned retry policy=%d %s", out.Code, out.Body.String())
	}
	var unchanged bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status='running' AND l.status='finalizing' AND l.terminal_at IS NULL FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id WHERE r.id=$1`, work.RunID).Scan(&unchanged); err != nil || !unchanged {
		t.Fatalf("invalid admission settled Task: %v %v", unchanged, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":false}' WHERE id=$1`, work.RunID)
	out = invoke(request)
	if out.Code != http.StatusNoContent {
		t.Fatalf("completion=%d %s", out.Code, out.Body.String())
	}
	var receipt string
	snapshot := func() string {
		t.Helper()
		var value string
		if err := f.Pool.QueryRow(t.Context(), `SELECT to_jsonb(l)::text FROM run_leases l WHERE id=$1`, work.LeaseID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	receipt = snapshot()
	worker.WorkerEpoch++
	out = invoke(request)
	if out.Code != http.StatusNoContent {
		t.Fatalf("previous-epoch replay=%d %s", out.Code, out.Body.String())
	}
	request.Outcome.Succeeded.Output = json.RawMessage(`{"secret":"must-not-be-logged"}`)
	out = invoke(request)
	if out.Code != http.StatusConflict || !strings.Contains(out.Body.String(), `"point":"replay"`) {
		t.Fatalf("changed replay=%d %s", out.Code, out.Body.String())
	}
	if snapshot() != receipt {
		t.Fatal("replay changed durable terminal receipt")
	}
	if !strings.Contains(logs.String(), "failure_point=execution") || !strings.Contains(logs.String(), "failure_point=replay") || strings.Contains(logs.String(), "must-not-be-logged") {
		t.Fatalf("incorrect diagnostics: %s", logs.String())
	}
}
