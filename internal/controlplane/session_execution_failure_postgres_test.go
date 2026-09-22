package controlplane

import (
	"encoding/json"
	"errors"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestSessionCheckpointFailureRequiresRecoveryPostgres(t *testing.T) {
	for _, active := range []bool{false, true} {
		name := "between turns"
		if active {
			name = "active Turn"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			f.turn(t, 1)
			next, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)})
			if err != nil {
				t.Fatal(err)
			}
			var scope session.TurnScope
			if active {
				scope = f.receiveTurn(t, 2)
			}
			// An enabled user retry policy cannot authorize replay of entered Actor code.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'::jsonb WHERE id=$1`, f.runID)
			reconciler, registration := actorTokenWait(t, f, scope)
			if !active {
				registration.TurnID = pgvalue.UUID(uuid.Nil())
				registration.TurnID.Valid = false
				registration.RunGeneration.Valid = false
			}
			registration.ActorSpeculativeInputSequence.Int64 = 1
			if active {
				registration.ActorSpeculativeInputSequence.Int64 = 2
			}
			if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
				t.Fatal(err)
			}
			lease, err := parseRunLeaseFence(f.fence())
			if err != nil {
				t.Fatal(err)
			}
			wait, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), lease, registration.WaitID)
			if err != nil {
				t.Fatal(err)
			}
			req := workerapi.CheckpointFailedRequest{Lease: f.fence(), RunWaitID: registration.WaitID.String(), CheckpointID: pgvalue.UUIDString(wait.SuspendCheckpointID), RequestVersion: wait.CheckpointRequestVersion, Error: "snapshot failed after side effect"}
			f.workerCall(t, f.server.workerMarkCheckpointFailed, req, nil)
			f.workerCall(t, f.server.workerMarkCheckpointFailed, req, nil)
			assertSessionExecutionHeld(t, f, f.rootID.String(), scope.TurnID, next.TurnID, "system_failed", "fenced")
			f.reportRuntimeClosed(t)
			var computer, dirty string
			var hold uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT w.status,w.dirty_state,s.dispatch_hold_id FROM workspaces w JOIN sessions s ON s.workspace_id=w.id WHERE s.id=$1`, f.sessionID).Scan(&computer, &dirty, &hold); err != nil {
				t.Fatal(err)
			}
			if computer != "recovery_required" || dirty != "dirty_state_lost" {
				t.Fatalf("Computer=%s/%s", computer, dirty)
			}
			var turnID *uuid.UUID
			disposition := ""
			if active {
				turnID = &scope.TurnID
				disposition = "failed"
			}
			// Process cleanup cannot make unpublished disk state durable. The
			// Computer must be reconciled before the Session can resume.
			if _, err := f.server.applySessionRecovery(t.Context(), session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, HoldID: hold}, TurnID: turnID, WorkspaceVersionID: f.rootID, ReconciliationRef: "runtime closed", Disposition: disposition}); err == nil {
				t.Fatal("runtime cleanup alone authorized stale disk recovery")
			} else {
				var operation *session.OperationError
				if !errors.As(err, &operation) || operation.Code != "not_settled" {
					t.Fatalf("recovery error=%v", err)
				}
			}
		})
	}
}

func TestSessionFailedCompletionRequiresRecoveryPostgres(t *testing.T) {
	for _, mode := range []string{"initialization", "active Turn"} {
		t.Run(mode, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			var active uuid.UUID
			if mode == "active Turn" {
				active = f.receiveTurn(t, 1).TurnID
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'::jsonb WHERE id=$1`, f.runID)
			a := f.claim
			operation := uuid.NewV7().String()
			begin := workerapi.BeginRunFinalizationRequest{Lease: f.fence(), ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: 1, RunLeaseID: f.fence().ID}, OperationID: operation, Kind: workerapi.RunFinalizationCapture}
			var began workerapi.BeginRunFinalizationResponse
			f.workerCall(t, f.server.workerBeginRunFinalization, begin, &began)
			assignment := workerapi.RunLeaseAssignment{ID: f.fence().ID, RunID: f.runID.String(), AttemptNumber: 1, LeaseSequence: f.fence().LeaseSequence, WorkerInstanceID: f.WorkerID.String(), WorkerEpoch: 1, RuntimeInstanceID: pgvalue.UUIDString(a.runtime.ID), RuntimeIdentityID: a.runtime.RuntimeIdentityID, WorkspaceID: f.workspaceID.String(), WorkspaceMountID: pgvalue.UUIDString(a.workspaceMount.ID), WorkspaceLeaseID: pgvalue.UUIDString(a.workspaceLease.ID), BaseWorkspaceVersionID: f.rootID.String(), OwnershipGeneration: a.workspace.OwnershipGeneration, WriterGeneration: a.workspace.WriterGeneration, MountFencingGeneration: a.workspaceMount.FencingGeneration, ExpiresAt: began.ExpiresAt}
			capture := validTaskWorkspaceCapture(t, assignment)
			content := f.capture(t, "retained after actor failure")
			capture.Receipt.OperationID = operation
			setCaptureFingerprint(t, capture)
			f.registerFinalizationDisk(t, capture, content.Artifact.Digest)
			req := workerapi.CompleteActorRequest{Lease: f.fence(), Outcome: workerapi.ActorOutcome{RunGeneration: f.claim.actor.RunGeneration, Failed: &workerapi.TaskFailure{Message: "initialization failed after side effect"}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
			parsed, err := parseActorCompletionRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatal(err)
			}
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatalf("failure replay: %v", err)
			}
			var queued uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM session_turns WHERE session_id=$1 AND sequence=1`, f.sessionID).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			retainedHead := assertRetainedActorCapture(t, f, capture, f.rootID)
			assertSessionExecutionHeld(t, f, retainedHead.String(), active, queued, "failed", "released")
			f.reportRuntimeClosed(t)
			assertSessionRecoveryCanResume(t, f)
		})
	}
}

func assertSessionExecutionHeld(t *testing.T, f *actorCheckpointFixture, head string, active, queued uuid.UUID, wantRun, wantWorkspaceLease string) {
	t.Helper()
	var state, reason, runState, leaseState, attemptState, workspaceLeaseState, runtimeDesired, turnState string
	var current, owner, actualHead uuid.UUID
	var actualActive *uuid.UUID
	var attempt, count, events int
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.dispatch_hold_reason,s.current_run_id,s.active_turn_id,w.owner_session_id,w.head_version_id,r.status,r.current_attempt_number,l.status,a.terminal_outcome,wl.status,rt.desired_state,(SELECT count(*) FROM run_attempts WHERE run_id=r.id),(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.held') FROM sessions s JOIN runs r ON r.id=s.current_run_id JOIN workspaces w ON w.id=s.workspace_id JOIN run_leases l ON l.id=$2 JOIN run_attempts a ON a.run_id=r.id AND a.number=r.current_attempt_number JOIN workspace_leases wl ON wl.owner_run_lease_id=l.id JOIN runtime_instances rt ON rt.id=l.runtime_instance_id WHERE s.id=$1`, f.sessionID, f.claim.runLease.ID).Scan(&state, &reason, &current, &actualActive, &owner, &actualHead, &runState, &attempt, &leaseState, &attemptState, &workspaceLeaseState, &runtimeDesired, &count, &events); err != nil {
		t.Fatal(err)
	}
	if state != "open" || reason != "recovery_required" || current != f.runID || owner != f.sessionID || actualHead.String() != head || runState != wantRun || attempt != 1 || count != 1 || events != 1 || leaseState != "failed" || attemptState != "failed" || workspaceLeaseState != wantWorkspaceLease || runtimeDesired != "closed" {
		t.Fatalf("failure convergence: %s %s run=%s/%s owner=%s head=%s attempt=%d/%d held=%d lease=%s/%s runtime=%s", state, reason, current, runState, owner, actualHead, attempt, count, events, leaseState, workspaceLeaseState, runtimeDesired)
	}
	if active == uuid.Nil() {
		if actualActive != nil {
			t.Fatalf("unexpected active Turn %s", actualActive)
		}
	} else if actualActive == nil || *actualActive != active {
		t.Fatal("active Turn changed")
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status FROM session_turns WHERE id=$1`, queued).Scan(&turnState); err != nil {
		t.Fatal(err)
	}
	wantTurn := "queued"
	if active == queued {
		wantTurn = "running"
	}
	if turnState != wantTurn {
		t.Fatalf("FIFO Turn state=%s want=%s", turnState, wantTurn)
	}
}

func assertSessionRecoveryCanResume(t *testing.T, f *actorCheckpointFixture) {
	t.Helper()
	var hold, head uuid.UUID
	var active *uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_id,w.head_version_id,s.active_turn_id FROM sessions s JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&hold, &head, &active); err != nil {
		t.Fatal(err)
	}
	disposition := ""
	if active != nil {
		disposition = "failed"
	}
	if _, err := f.server.applySessionRecovery(t.Context(), session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, HoldID: hold}, TurnID: active, WorkspaceVersionID: head, ReconciliationRef: "test physical close", Disposition: disposition}); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.applySessionResume(t.Context(), session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, HoldID: hold}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	var current uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT current_run_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current == f.runID {
		t.Fatal("resume revived failed Run")
	}
}

func TestSessionSuccessfulReturnPreservesPendingWorkPostgres(t *testing.T) {
	for _, pending := range []bool{true, false} {
		name := "drained"
		if pending {
			name = "no progress"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			if !pending {
				f.turn(t, 1)
			}
			var head uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id FROM workspaces WHERE id=$1`, f.workspaceID).Scan(&head); err != nil {
				t.Fatal(err)
			}
			a := f.claim
			operation := uuid.NewV7().String()
			var began workerapi.BeginRunFinalizationResponse
			f.workerCall(t, f.server.workerBeginRunFinalization, workerapi.BeginRunFinalizationRequest{Lease: f.fence(), ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: 1, RunLeaseID: f.fence().ID}, OperationID: operation, Kind: workerapi.RunFinalizationCapture}, &began)
			assignment := workerapi.RunLeaseAssignment{ID: f.fence().ID, RunID: f.runID.String(), AttemptNumber: 1, LeaseSequence: f.fence().LeaseSequence, WorkerInstanceID: f.WorkerID.String(), WorkerEpoch: 1, RuntimeInstanceID: pgvalue.UUIDString(a.runtime.ID), RuntimeIdentityID: a.runtime.RuntimeIdentityID, WorkspaceID: f.workspaceID.String(), WorkspaceMountID: pgvalue.UUIDString(a.workspaceMount.ID), WorkspaceLeaseID: pgvalue.UUIDString(a.workspaceLease.ID), BaseWorkspaceVersionID: head.String(), OwnershipGeneration: a.workspace.OwnershipGeneration, WriterGeneration: a.workspace.WriterGeneration, MountFencingGeneration: a.workspaceMount.FencingGeneration, ExpiresAt: began.ExpiresAt}
			capture := validTaskWorkspaceCapture(t, assignment)
			content := f.capture(t, "returned without more input")
			capture.Receipt.OperationID = operation
			setCaptureFingerprint(t, capture)
			f.registerFinalizationDisk(t, capture, content.Artifact.Digest)
			req := workerapi.CompleteActorRequest{Lease: f.fence(), Outcome: workerapi.ActorOutcome{RunGeneration: f.claim.actor.RunGeneration, Succeeded: &workerapi.ActorSucceeded{}}, Workspace: workerapi.TaskWorkspaceProof{Captured: capture}}
			parsed, err := parseActorCompletionRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatal(err)
			}
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatalf("completion replay: %v", err)
			}

			if pending {
				var queued uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT id FROM session_turns WHERE session_id=$1 AND sequence=1`, f.sessionID).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				retainedHead := assertRetainedActorCapture(t, f, capture, head)
				assertSessionExecutionHeld(t, f, retainedHead.String(), uuid.Nil(), queued, "failed", "released")
				var reason string
				if err := f.Pool.QueryRow(t.Context(), `SELECT terminal_reason_code FROM run_attempts WHERE run_id=$1 AND number=1`, f.runID).Scan(&reason); err != nil {
					t.Fatal(err)
				}
				if reason != "no_progress" {
					t.Fatalf("reason=%s", reason)
				}
			} else {
				var state, runState string
				var current, hold *uuid.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.current_run_id,s.dispatch_hold_id,r.status FROM sessions s JOIN runs r ON r.id=$2 WHERE s.id=$1`, f.sessionID, f.runID).Scan(&state, &current, &hold, &runState); err != nil {
					t.Fatal(err)
				}
				if state != "open" || runState != "succeeded" || current != nil || hold != nil {
					t.Fatalf("clean return=%s %s current=%v hold=%v", state, runState, current, hold)
				}
			}
		})
	}
}

func assertRetainedActorCapture(t *testing.T, f *actorCheckpointFixture, capture *workerapi.TaskWorkspaceCapture, previous uuid.UUID) uuid.UUID {
	t.Helper()
	var head uuid.UUID
	var digest string
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_version_id, a.digest FROM workspaces w JOIN workspace_versions v ON v.id=w.head_version_id JOIN artifacts a ON a.id=v.artifact_id WHERE w.id=$1`, f.workspaceID).Scan(&head, &digest); err != nil {
		t.Fatal(err)
	}
	if head == previous || digest != capture.Disk.Artifact.Digest {
		t.Fatalf("failure capture not retained: head=%s digest=%s", head, digest)
	}
	return head
}
