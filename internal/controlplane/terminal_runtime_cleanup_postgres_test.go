package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

// Completion commits the saved Computer, but reclamation still needs an exact
// Worker acknowledgement of physical closure.
func assertTerminalRuntimeCleanup(t *testing.T, server *Server, pool db.DBTX, worker workerActor, leaseID string) {
	t.Helper()
	var runtimeID, desired, observed, mountStatus string
	var desiredVersion, observedVersion int64
	var reclaimed bool
	read := func() {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT ri.id,ri.desired_state,ri.observed_state,ri.desired_version,ri.observed_version,ri.reclaimed_at IS NOT NULL,wm.status
 FROM run_leases rl JOIN runtime_instances ri ON ri.id=rl.runtime_instance_id
 JOIN workspace_mounts wm ON wm.runtime_instance_id=ri.id WHERE rl.id=$1`, leaseID).Scan(&runtimeID, &desired, &observed, &desiredVersion, &observedVersion, &reclaimed, &mountStatus); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if desired != "closed" || observed != "ready" || reclaimed || mountStatus != "unmounting" {
		t.Fatalf("terminal intent: runtime=%s/%s reclaimed=%v mount=%s", desired, observed, reclaimed, mountStatus)
	}
	request := workerapi.RuntimeInstanceStateRequest{ID: runtimeID, WorkerEpoch: worker.WorkerEpoch, DesiredVersion: desiredVersion, ExpectedObservedVersion: observedVersion, CleanupProof: &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()}}
	call := func(request workerapi.RuntimeInstanceStateRequest) int {
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/worker/v1/runtime/closed", bytes.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), workerContextKey{}, worker))
		response := httptest.NewRecorder()
		server.workerMarkRuntimeInstanceClosed(response, req)
		return response.Code
	}
	missing := request
	missing.CleanupProof = nil
	if status := call(missing); status != http.StatusBadRequest {
		t.Fatalf("missing proof status=%d", status)
	}
	stale := request
	stale.DesiredVersion++
	if status := call(stale); status != http.StatusConflict {
		t.Fatalf("stale cleanup status=%d", status)
	}
	read()
	if reclaimed || mountStatus != "unmounting" {
		t.Fatal("stale close proof released authority")
	}
	if status := call(request); status != http.StatusOK {
		t.Fatalf("exact cleanup status=%d", status)
	}
	read()
	if observed != "closed" || !reclaimed || mountStatus != "unmounted" {
		t.Fatalf("physical cleanup: runtime=%s reclaimed=%v mount=%s", observed, reclaimed, mountStatus)
	}
}

func TestRootTaskCompletionReclaimsRuntimeAfterPhysicalProof(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "retry"} {
		t.Run(outcome, func(t *testing.T) {
			base := runtest.New(t)
			work := base.AddRunLease(t, "starting", time.Now().Add(-time.Minute))
			ctx := t.Context()
			var workspaceID, versionID, runtimeID, mountID, workspaceLeaseID uuid.UUID
			if err := base.Pool.QueryRow(ctx, `SELECT r.workspace_id,r.base_workspace_version_id,l.runtime_instance_id,wl.workspace_mount_id,wl.id FROM runs r JOIN run_leases l ON l.id=r.current_run_lease_id JOIN workspace_leases wl ON wl.owner_run_lease_id=l.id WHERE r.id=$1`, work.RunID).Scan(&workspaceID, &versionID, &runtimeID, &mountID, &workspaceLeaseID); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, ctx, base.Pool, `UPDATE runs SET status='running',started_at=now(),active_started_at=now(),revision=3 WHERE id=$1`, work.RunID)
			dbtest.MustExec(t, ctx, base.Pool, `UPDATE run_leases SET status='running',started_at=now() WHERE id=$1`, work.LeaseID)
			dbtest.MustExec(t, ctx, base.Pool, `UPDATE run_attempts SET entrypoint_entered_at=now() WHERE run_id=$1`, work.RunID)
			policy := `{"enabled":false}`
			if outcome == "retry" {
				policy = `{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`
			}
			dbtest.MustExec(t, ctx, base.Pool, `UPDATE runs SET retry_policy=$2::jsonb WHERE id=$1`, work.RunID, policy)
			server := &Server{db: db.New(base.Pool), tx: base.Pool, cas: finalizationTestCAS(t), log: taskCompletionTestLogger()}
			worker := workerActor{WorkerInstanceID: base.WorkerID, WorkerGroupID: runtest.WorkerGroupID, WorkerEpoch: 1, ClaimVersion: 1, GroupClaimVersion: 1}
			assignment := workerapi.RunLeaseAssignment{ID: work.LeaseID.String(), RunID: work.RunID.String(), AttemptNumber: 1, LeaseSequence: 1, WorkerGroupID: runtest.WorkerGroup, WorkerInstanceID: base.WorkerID.String(), WorkerEpoch: 1, RuntimeInstanceID: runtimeID.String(), RuntimeIdentityID: base.RuntimeIdentityID, WorkspaceID: workspaceID.String(), WorkspaceMountID: mountID.String(), WorkspaceLeaseID: workspaceLeaseID.String(), BaseWorkspaceVersionID: versionID.String(), OwnershipGeneration: 1, WriterGeneration: 1, MountFencingGeneration: 2}
			begin := workerapi.BeginRunFinalizationRequest{Lease: assignment.Fence(), OperationID: uuid.NewV7().String(), Kind: workerapi.RunFinalizationCapture, ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: work.RunID.String(), AttemptNumber: 1, RunLeaseID: work.LeaseID.String()}}
			parsedBegin, err := parseRunFinalization(begin)
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := server.beginRunFinalization(ctx, worker, begin, parsedBegin)
			if err != nil {
				t.Fatal(err)
			}
			assignment.ExpiresAt = frozen.ExpiresAt
			capture := validTaskWorkspaceCapture(t, assignment)
			capture.Receipt.OperationID = frozen.OperationID
			setCaptureFingerprint(t, capture)
			registerFinalizationTestDisk(t, base.Pool, server, worker, assignment.Fence(), capture, frozen.OperationID)
			request := workerapi.CompleteTaskRequest{Lease: assignment.Fence(), Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{Output: json.RawMessage(`{"ok":true}`)}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
			if outcome != "success" {
				request.Outcome = workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "failed task"}}
			}
			parsed, err := parseTaskCompletionRequest(request)
			if err != nil {
				t.Fatal(err)
			}
			if err = server.completeTask(ctx, worker, request, parsed); err != nil {
				t.Fatal(err)
			}
			assertTerminalRuntimeCleanup(t, server, base.Pool, worker, work.LeaseID.String())
			// A replay after physical reclamation retains the same terminal receipt.
			if err = server.completeTask(ctx, worker, request, parsed); err != nil {
				t.Fatal(err)
			}
			var status string
			if err = base.Pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, work.RunID).Scan(&status); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"success": "succeeded", "failure": "failed", "retry": "retry_delayed"}[outcome]
			if status != want {
				t.Fatalf("run=%s want=%s", status, want)
			}
		})
	}
}

func TestRuntimeCloseProofDoesNotCompleteCaptureMount(t *testing.T) {
	f := newWorkspaceDeletionPhysicalFixture(t)
	dbtest.MustExec(t, t.Context(), f.base.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtimeID)
	dbtest.MustExec(t, t.Context(), f.base.Pool, `UPDATE workspace_mounts SET status='unmounting',finalization_action='capture',finalization_reason_code='workspace_exec_completed',stopped_at=now() WHERE id=$1`, f.mountID)
	var desired, observed int64
	if err := f.base.Pool.QueryRow(t.Context(), `SELECT desired_version,observed_version FROM runtime_instances WHERE id=$1`, f.runtimeID).Scan(&desired, &observed); err != nil {
		t.Fatal(err)
	}
	_, err := db.New(f.base.Pool).MarkRuntimeInstanceClosed(t.Context(), db.MarkRuntimeInstanceClosedParams{ID: pgvalue.UUID(f.runtimeID), WorkerInstanceID: pgvalue.UUID(f.base.WorkerID), WorkerEpoch: 1, DesiredVersion: desired, ExpectedObservedVersion: observed, ReasonCode: pgvalue.Text("test_closed"), CleanupProof: []byte(`{"method":"session_closed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var status, action string
	var terminal bool
	if err = f.base.Pool.QueryRow(t.Context(), `SELECT status,finalization_action,terminal_at IS NOT NULL FROM workspace_mounts WHERE id=$1`, f.mountID).Scan(&status, &action, &terminal); err != nil {
		t.Fatal(err)
	}
	if status != "unmounting" || action != "capture" || terminal {
		t.Fatalf("capture overwritten: %s/%s terminal=%v", status, action, terminal)
	}
}
