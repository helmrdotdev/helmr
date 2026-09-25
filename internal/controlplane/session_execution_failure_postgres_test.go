package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestSessionCheckpointFailureAutomaticallyReconcilesPostgres(t *testing.T) {
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
			var computer, dirty string
			var hold uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT w.status,w.dirty_state,s.dispatch_hold_id FROM computers w JOIN sessions s ON s.workspace_id=w.id WHERE s.id=$1`, f.sessionID).Scan(&computer, &dirty, &hold); err != nil {
				t.Fatal(err)
			}
			if computer != "recovery_required" || dirty != "dirty_state_lost" {
				t.Fatalf("Computer=%s/%s", computer, dirty)
			}
			lifecycle, _ := session.NewReconciler(f.Pool)
			if deferred, err := lifecycle.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || !deferred {
				t.Fatalf("before physical cleanup=%v %v", deferred, err)
			}
			f.reportRuntimeClosed(t)
			assertSessionAutomaticallyReconciles(t, f)
			var cursor int64
			var savedHead uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT w.status,w.dirty_state,w.head_version_id,s.committed_input_sequence FROM computers w JOIN sessions s ON s.workspace_id=w.id WHERE s.id=$1`, f.sessionID).Scan(&computer, &dirty, &savedHead, &cursor); err != nil {
				t.Fatal(err)
			}
			expectedCursor := int64(1)
			if active {
				expectedCursor = 2
			}
			if computer != "active" || dirty != "clean" || savedHead != f.rootID || cursor != expectedCursor {
				t.Fatalf("recovered Computer=%s/%s head=%s cursor=%d", computer, dirty, savedHead, cursor)
			}

		})
	}
}

func TestSessionFailedCompletionTerminatesSessionPostgres(t *testing.T) {
	for _, mode := range []string{"initialization", "active Turn"} {
		t.Run(mode, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			var active uuid.UUID
			if mode == "active Turn" {
				active = f.receiveTurn(t, 1).TurnID
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_policy='{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}'::jsonb WHERE id=$1`, f.runID)
			req, parsed := failedActorCompletion(t, f)
			capture := req.Workspace.Captured
			var before, after string
			snapshot := `SELECT jsonb_build_object('session',to_jsonb(s),'run',to_jsonb(r),'head',c.head_version_id)::text FROM sessions s JOIN runs r ON r.id=$2 JOIN computers c ON c.id=s.workspace_id WHERE s.id=$1`
			if err := f.Pool.QueryRow(t.Context(), snapshot, f.sessionID, f.runID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_session_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.kind='session.failed' THEN RAISE EXCEPTION 'injected event failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_session_failure BEFORE INSERT ON session_events FOR EACH ROW EXECUTE FUNCTION reject_session_failure()`)
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err == nil {
				t.Fatal("event failure ignored")
			}
			if err := f.Pool.QueryRow(t.Context(), snapshot, f.sessionID, f.runID).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if after != before {
				t.Fatal("failure partially committed")
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `DROP TRIGGER reject_session_failure ON session_events`)
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatal(err)
			}
			if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatalf("failure replay: %v", err)
			}
			assertRetainedActorCapture(t, f, capture, f.rootID)
			assertFailedActorSession(t, f, active != uuid.Nil())
			if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"later":true}`)}); err == nil {
				t.Fatal("failed Session accepted new input")
			}
		})
	}
}

func assertSessionExecutionHeld(t *testing.T, f *actorCheckpointFixture, head string, active, queued uuid.UUID, wantRun, wantWorkspaceLease string) {
	t.Helper()
	var state, reason, runState, leaseState, attemptState, workspaceLeaseState, runtimeDesired, turnState string
	var current, owner, actualHead uuid.UUID
	var actualActive *uuid.UUID
	var attempt, count, events int
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.dispatch_hold_reason,s.current_run_id,s.active_turn_id,w.owner_session_id,w.head_version_id,r.status,r.current_attempt_number,l.status,a.terminal_outcome,wl.status,rt.desired_state,(SELECT count(*) FROM run_attempts WHERE run_id=r.id),(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.held') FROM sessions s JOIN runs r ON r.id=s.current_run_id JOIN computers w ON w.id=s.workspace_id JOIN run_leases l ON l.id=$2 JOIN run_attempts a ON a.run_id=r.id AND a.number=r.current_attempt_number JOIN workspace_leases wl ON wl.owner_run_lease_id=l.id JOIN runtime_instances rt ON rt.id=l.runtime_instance_id WHERE s.id=$1`, f.sessionID, f.claim.runLease.ID).Scan(&state, &reason, &current, &actualActive, &owner, &actualHead, &runState, &attempt, &leaseState, &attemptState, &workspaceLeaseState, &runtimeDesired, &count, &events); err != nil {
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

func assertSessionAutomaticallyReconciles(t *testing.T, f *actorCheckpointFixture) {
	t.Helper()
	lifecycle, _ := session.NewReconciler(f.Pool)
	for range 2 {
		if deferred, err := lifecycle.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || deferred {
			t.Fatalf("automatic reconciliation=%v %v", deferred, err)
		}
	}
	var fresh bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT current_run_id IS DISTINCT FROM $2 AND dispatch_hold_id IS NULL FROM sessions WHERE id=$1`, f.sessionID, f.runID).Scan(&fresh); err != nil || !fresh {
		t.Fatalf("old execution retained=%v %v", fresh, err)
	}
}

func TestSessionSuccessfulReturnPreservesPendingWorkPostgres(t *testing.T) {
	for _, name := range []string{"no progress", "drained", "input after idle admission"} {
		t.Run(name, func(t *testing.T) {
			pending := name == "no progress"
			late := name == "input after idle admission"
			var input json.RawMessage
			if !late {
				input = json.RawMessage(`{"sequence":1}`)
			}
			f := newActorCheckpointFixtureWithInput(t, input)
			if pending {
				// Seed the immutable admission frontier. The fixture normally
				// enqueues after creating its boot Run, whose frontier is empty.
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET session_input_high_watermark=1 WHERE id=$1`, f.runID)
			}
			if late {
				if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{
					Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID},
					Mode:   session.EnqueueOnly, Data: json.RawMessage(`{"sequence":1}`),
				}); err != nil {
					t.Fatal(err)
				}
			} else if !pending {
				f.turn(t, 1)
			}
			var head uuid.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id FROM computers WHERE id=$1`, f.workspaceID).Scan(&head); err != nil {
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
				assertRetainedActorCapture(t, f, capture, head)
				assertFailedActorSession(t, f, false)
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
				if state != "open" || runState != "succeeded" || (current != nil) != late || hold != nil {
					t.Fatalf("clean return=%s %s current=%v hold=%v", state, runState, current, hold)
				}
				if late {
					var status string
					var cursor int64
					if err := f.Pool.QueryRow(t.Context(), `SELECT t.status,s.committed_input_sequence FROM sessions s JOIN session_turns t ON t.session_id=s.id AND t.sequence=1 WHERE s.id=$1`, f.sessionID).Scan(&status, &cursor); err != nil {
						t.Fatal(err)
					}
					if *current == f.runID || status != "queued" || cursor != 0 {
						t.Fatalf("late input lost: run=%v status=%s cursor=%d", current, status, cursor)
					}
				}
			}
		})
	}
}

func assertRetainedActorCapture(t *testing.T, f *actorCheckpointFixture, capture *workerapi.TaskWorkspaceCapture, previous uuid.UUID) uuid.UUID {
	t.Helper()
	var head uuid.UUID
	var digest string
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.head_version_id, r.root_digest FROM computers w JOIN computer_versions v ON v.id=w.head_version_id JOIN computer_version_roots r ON r.version_id=v.id WHERE w.id=$1`, f.workspaceID).Scan(&head, &digest); err != nil {
		t.Fatal(err)
	}
	if head == previous || digest != capture.Disk.Root.Pack.Digest {
		t.Fatalf("failure capture not retained: head=%s digest=%s", head, digest)
	}
	return head
}

func assertFailedActorSession(t *testing.T, f *actorCheckpointFixture, active bool) {
	t.Helper()
	var state, runState, turnState string
	var current, hold, owner, failureRun *uuid.UUID
	var events, runs int
	var cursor int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.status,s.current_run_id,s.dispatch_hold_id,c.owner_session_id,s.failure_run_id,s.committed_input_sequence,r.status,t.status,(SELECT count(*) FROM session_events WHERE session_id=s.id AND kind='session.failed'),(SELECT count(*) FROM runs WHERE session_id=s.id) FROM sessions s JOIN computers c ON c.id=s.workspace_id JOIN runs r ON r.id=$2 JOIN session_turns t ON t.session_id=s.id AND t.sequence=1 WHERE s.id=$1`, f.sessionID, f.runID).Scan(&state, &current, &hold, &owner, &failureRun, &cursor, &runState, &turnState, &events, &runs); err != nil {
		t.Fatal(err)
	}
	wantTurn, wantCursor := "cancelled", int64(0)
	if active {
		wantTurn, wantCursor = "failed", 1
	}
	if state != "failed" || current != nil || hold != nil || owner != nil || failureRun == nil || *failureRun != f.runID || runState != "failed" || turnState != wantTurn || cursor != wantCursor || events != 1 || runs != 1 {
		t.Fatalf("failure state=%s current=%v hold=%v owner=%v failureRun=%v run=%s turn=%s cursor=%d events=%d runs=%d", state, current, hold, owner, failureRun, runState, turnState, cursor, events, runs)
	}
}

// failedActorCompletion freezes and registers the quiesced worker's receipt
// before a competing external stop operation can be admitted.
func failedActorCompletion(t *testing.T, f *actorCheckpointFixture) (workerapi.CompleteActorRequest, parsedActorCompletion) {
	t.Helper()
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
	return req, parsed
}

func TestSessionStopRacingFailedCompletionPostgres(t *testing.T) {
	for _, mode := range []string{"initialization cancel", "returned cancel", "active cancel", "active interrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
			var active uuid.UUID
			if mode == "active cancel" || mode == "active interrupt" {
				active = f.receiveTurn(t, 1).TurnID
			}
			queued, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"next":true}`)})
			if err != nil {
				t.Fatal(err)
			}
			req, parsed := failedActorCompletion(t, f)
			if mode == "returned cancel" {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET session_input_high_watermark=1 WHERE id=$1`, f.runID)
				req.Outcome.Failed = nil
				req.Outcome.Succeeded = &workerapi.ActorSucceeded{}
				parsed, err = parseActorCompletionRequest(req)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "active interrupt" {
				_, err = f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: target}, TurnID: active})
			} else {
				_, err = f.server.applySessionCancel(t.Context(), session.ControlRequest{Target: target})
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatal(err)
			}
			if err = f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
				t.Fatalf("receipt replay: %v", err)
			}
			var status, reason, runStatus, queuedStatus string
			var current *uuid.UUID
			if err = f.Pool.QueryRow(t.Context(), `SELECT s.status,s.dispatch_hold_reason,s.current_run_id,r.status,t.status FROM sessions s JOIN runs r ON r.id=$2 JOIN session_turns t ON t.id=$3 WHERE s.id=$1`, f.sessionID, f.runID, queued.TurnID).Scan(&status, &reason, &current, &runStatus, &queuedStatus); err != nil {
				t.Fatal(err)
			}
			wantStatus, wantQueue := "closing", "cancelled"
			if mode == "active interrupt" {
				wantStatus, wantQueue = "open", "queued"
			}
			if status != wantStatus || reason != "interrupted" || current != nil || runStatus != "cancelled" || queuedStatus != wantQueue {
				t.Fatalf("session=%s hold=%s current=%v run=%s queue=%s", status, reason, current, runStatus, queuedStatus)
			}
			if active != uuid.Nil() {
				var turnStatus string
				if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM session_turns WHERE id=$1`, active).Scan(&turnStatus); err != nil || turnStatus != "interrupted" {
					t.Fatalf("turn=%s err=%v", turnStatus, err)
				}
			}
			if mode != "active interrupt" {
				f.reportRuntimeClosed(t)
				r, err := session.NewReconciler(f.Pool)
				if err != nil {
					t.Fatal(err)
				}
				if waiting, err := r.ReconcileLifecycle(t.Context(), f.EnvironmentID, f.sessionID); err != nil || waiting {
					t.Fatalf("cancel close waiting=%v err=%v", waiting, err)
				}
			}
		})
	}
}

func TestSessionFailedCompletionPreservesHistoryPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	f.turn(t, 1)
	target := session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}
	for i := 0; i < 3; i++ {
		if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: target, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"work":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	scope := f.receiveTurn(t, 2)
	readyMessages(t, f, scope)
	admitMessage(t, f, "handled")
	delivered := claimMessage(t, f, scope)
	finishDelivery(t, f, scope, delivered, "handled", "")
	admitMessage(t, f, "handling")
	claimMessage(t, f, scope)
	admitMessage(t, f, "queued")

	var before, after string
	query := `SELECT to_jsonb(t)::text FROM session_turns t WHERE session_id=$1 AND sequence=1`
	if err := f.Pool.QueryRow(t.Context(), query, f.sessionID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	req, parsed := failedActorCompletion(t, f)
	if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatal(err)
	}
	if err := f.server.completeActor(t.Context(), f.worker, req, parsed); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), query, f.sessionID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("previous completed Turn changed")
	}
	var turns, messages []string
	if err := f.Pool.QueryRow(t.Context(), `SELECT array_agg(status ORDER BY sequence) FROM session_turns WHERE session_id=$1`, f.sessionID).Scan(&turns); err != nil {
		t.Fatal(err)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT array_agg(status ORDER BY accepted_sequence) FROM session_messages WHERE session_id=$1`, f.sessionID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if len(turns) != 4 || turns[0] != "completed" || turns[1] != "failed" || turns[2] != "cancelled" || turns[3] != "cancelled" {
		t.Fatalf("turns=%v", turns)
	}
	if len(messages) != 3 || messages[0] != "handled" || messages[1] != "unknown" || messages[2] != "rejected" {
		t.Fatalf("messages=%v", messages)
	}
}
