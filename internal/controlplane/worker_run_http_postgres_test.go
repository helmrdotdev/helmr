package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// runLeaseHTTPFixture serves NewServer to the worker host of an assigned
// lease.
func runLeaseHTTPFixture(t *testing.T) (runtest.Fixture, runtest.RunLease, workerHTTPClient) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	return f, work, newWorkerHTTPClient(t, newPostgresServer(t, f.Pool), f.Pool, f.WorkerID)
}

// runLeaseRouteRequests are a request to each worker Run lease route under
// the receipt.
func runLeaseRouteRequests(work runtest.RunLease, lease workerapi.RunLeaseFence) map[string]any {
	return map[string]any{
		"/worker/v1/run/leases/claim":      workerapi.RunLeaseClaimRequest{LeaseID: lease.ID, LeaseSequence: lease.LeaseSequence},
		"/worker/v1/run/leases/start":      workerapi.RunStartRequest{Lease: lease},
		"/worker/v1/run/leases/entrypoint": workerapi.RunEntrypointRequest{Lease: lease, EntrypointKind: "task", EntrypointDeclaredID: "test-task"},
		"/worker/v1/run/leases/renew":      workerapi.RunLeaseRenewRequest{Lease: lease, ExpectedExpiresAt: time.Now()},
		"/worker/v1/run/finalization/begin": workerapi.BeginRunFinalizationRequest{Lease: lease, OperationID: uuid.NewV7().String(), ProgramQuiesced: workerapi.RunQuiescenceProof{
			RunID: work.RunID.String(), AttemptNumber: 1, RunLeaseID: lease.ID,
		}},
		"/worker/v1/run/tasks/complete":         workerapi.CompleteTaskRequest{Lease: lease, OperationID: uuid.NewV7().String(), Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{Output: json.RawMessage(`{}`)}}},
		"/worker/v1/run/logs/append":            workerapi.RunLogAppendRequest{Lease: lease, Stream: workerapi.LogStreamStdout, ObservedSeq: 1, ContentBase64: base64.StdEncoding.EncodeToString([]byte("alpha"))},
		"/worker/v1/run/structured-logs/append": workerapi.StructuredLogRequest{Lease: lease, ObservedSeq: 1, Level: "info", Message: "alpha", Attributes: json.RawMessage(`{}`)},
		"/worker/v1/run/metadata/update":        workerapi.UpdateRunMetadataRequest{Lease: lease, OperationID: uuid.NewV7().String(), Operation: "set", Key: "phase", Value: json.RawMessage(`"running"`)},
		"/worker/v1/run/waits/resume-ack":       workerapi.RunWaitResumeAckRequest{Lease: lease, RunWaitID: uuid.NewV7().String(), CheckpointID: uuid.NewV7().String()},
	}
}

// Worker Run lease routes answer a receipt that addresses no live execution
// through the run owner as a conflict in each route's vocabulary.
func TestWorkerRunLeaseRoutesMapStaleReceipts(t *testing.T) {
	_, work, worker := runLeaseHTTPFixture(t)
	stale := workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 2}
	want := map[string]string{
		"/worker/v1/run/leases/claim":           `"message":"run lease claim is stale"`,
		"/worker/v1/run/leases/start":           `"code":"run_start_stale","message":"run start authority is stale","details":{"point":"execution"}`,
		"/worker/v1/run/leases/entrypoint":      `"message":"run entrypoint acknowledgement is stale"`,
		"/worker/v1/run/leases/renew":           `"message":"worker run lease fence is stale"`,
		"/worker/v1/run/finalization/begin":     `"message":"run finalization authority is stale"`,
		"/worker/v1/run/tasks/complete":         `"code":"task_completion_stale","message":"task completion authority is stale","details":{"point":"execution"}`,
		"/worker/v1/run/logs/append":            `"message":"worker run lease is stale or the log chunk sequence contains different content"`,
		"/worker/v1/run/structured-logs/append": `"message":"worker run lease is stale or the structured log sequence contains different content"`,
		"/worker/v1/run/metadata/update":        `"message":"worker run lease fence is stale"`,
		"/worker/v1/run/waits/resume-ack":       `"message":"run wait resume acknowledgement is stale"`,
	}
	for path, body := range runLeaseRouteRequests(work, stale) {
		t.Run(path, func(t *testing.T) {
			out := worker.post(t, path, body, http.StatusConflict, nil)
			if !strings.Contains(out.Body.String(), want[path]) {
				t.Fatalf("body = %s, want %s", out.Body.String(), want[path])
			}
		})
	}
}

// Worker Run lease routes that compare credential claims ask a host whose
// claims went stale to re-authenticate.
func TestWorkerRunLeaseRoutesRejectStaleClaims(t *testing.T) {
	f, work, worker := runLeaseHTTPFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.WorkerID)
	lease := workerapi.RunLeaseFence{ID: work.LeaseID.String(), LeaseSequence: 1}
	requests := runLeaseRouteRequests(work, lease)
	for _, path := range []string{"/worker/v1/run/leases/claim", "/worker/v1/run/leases/start"} {
		t.Run(path, func(t *testing.T) {
			worker.post(t, path, requests[path], http.StatusUnauthorized, nil)
		})
	}
}
