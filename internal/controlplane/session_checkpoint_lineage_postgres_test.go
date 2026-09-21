package controlplane

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func checkpointTokenAndResume(t *testing.T, f *actorCheckpointFixture, scope session.TurnScope, cursor int64, capture testWorkspaceCapture) workerapi.CheckpointResponse {
	t.Helper()
	reconciler, registration := actorTokenWait(t, f, scope)
	registration.ActorSpeculativeInputSequence.Int64 = cursor
	if scope.TurnID == uuid.Nil() {
		registration.TurnID = pgtype.UUID{}
		registration.RunGeneration = pgtype.Int8{}
	}
	if f.claim.run.EntrypointKind == "task" {
		registration.ActorSpeculativeInputSequence = pgtype.Int8{}
	}
	if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	checkpoint := f.suspendWait(t, registration.WaitID, capture)
	if _, err := f.server.db.CompleteToken(t.Context(), db.CompleteTokenParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(registration.TokenID), CompletionFingerprint: make([]byte, 32), Result: []byte(`true`), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}); err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, registration.TokenID, 1); err != nil || result.Resolved != 1 {
		t.Fatalf("Token resolve: %+v %v", result, err)
	}
	f.placeAndClaim(t)
	f.startClaim(t)
	return checkpoint
}

func TestSessionRepeatedCheckpointLineagePostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "committed and private contents")
	first := f.turn(t, 1, capture, true)
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)}); err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, 2)
	c1 := checkpointTokenAndResume(t, f, scope, 2, capture)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_checkpoints SET expires_at=transaction_timestamp()-interval '1 second' WHERE id=$1`, uuid.MustParse(c1.CheckpointID))
	c2 := checkpointTokenAndResume(t, f, scope, 2, capture)
	var base uuid.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT base_workspace_version_id FROM run_checkpoints WHERE id=$1`, uuid.MustParse(c2.CheckpointID)).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base.String() != c1.WorkspaceVersionID || base.String() == first.WorkspaceVersionID {
		t.Fatalf("second checkpoint base=%s first=%s head=%s", base, c1.WorkspaceVersionID, first.WorkspaceVersionID)
	}
	params := db.ActorCheckpointLineageIsValidParams{RunID: pgvalue.UUID(f.runID), AttemptNumber: 1, WorkspaceID: pgvalue.UUID(f.workspaceID), CheckpointID: pgvalue.UUID(uuid.MustParse(c2.CheckpointID)), CommittedHeadVersionID: pgvalue.UUID(uuid.MustParse(first.WorkspaceVersionID)), OwnershipGeneration: f.claim.workspace.OwnershipGeneration}
	for _, test := range []struct {
		name, query string
		arg         uuid.UUID
	}{
		{"broken restore pointer", `UPDATE runtime_instances SET restore_checkpoint_id=NULL WHERE id=(SELECT runtime_instance_id FROM run_leases WHERE id=(SELECT source_run_lease_id FROM run_checkpoints WHERE id=$1))`, uuid.MustParse(c2.CheckpointID)},
		{"unacknowledged prior restore", `UPDATE run_waits SET resume_ack_version=0 WHERE id=(SELECT run_wait_id FROM run_checkpoints WHERE id=$1)`, uuid.MustParse(c1.CheckpointID)},
		{"wrong private parent", `UPDATE workspace_versions SET parent_version_id=(SELECT head_version_id FROM workspaces WHERE id=workspace_versions.workspace_id) WHERE id=$1`, uuid.MustParse(c2.WorkspaceVersionID)},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), test.query, test.arg); err != nil {
				t.Fatal(err)
			}
			valid, err := db.New(tx).ActorCheckpointLineageIsValid(t.Context(), params)
			if err != nil || valid {
				t.Fatalf("invalid lineage accepted=%v err=%v", valid, err)
			}
		})
	}
	wrongRun := params
	wrongRun.RunID = pgvalue.UUID(uuid.NewV7())
	if valid, err := f.server.db.ActorCheckpointLineageIsValid(t.Context(), wrongRun); err != nil || valid {
		t.Fatalf("wrong execution accepted=%v err=%v", valid, err)
	}
	settled := f.turn(t, 2, capture, false)
	if settled.WorkspaceVersionID != c2.WorkspaceVersionID {
		t.Fatalf("unchanged settlement=%s want %s", settled.WorkspaceVersionID, c2.WorkspaceVersionID)
	}
}

func TestSessionOutsideTurnCheckpointThenTurnSettlementPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "between turns")
	f.turn(t, 1, capture, true)
	checkpointTokenAndResume(t, f, session.TurnScope{}, 1, capture)
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)}); err != nil {
		t.Fatal(err)
	}
	f.receiveTurn(t, 2)
	f.turn(t, 2, capture, false)
}

func checkpointChildAndResume(t *testing.T, f *actorCheckpointFixture, scope session.TurnScope, capture testWorkspaceCapture, outcome string) testWorkspaceCapture {
	t.Helper()
	manifestInput := `{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`
	if outcome == "retry cancellation" || outcome == "retry exhaustion" {
		manifestInput = `{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":true,"maxAttempts":2,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}}}`
	}
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(manifestInput))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, manifest, digest[:])
	turnID := scope.TurnID.String()
	cursor := int64(2)
	target, _ := json.Marshal(map[string]string{"id": f.workspaceID.String()})
	req := workerapi.InvokeChildTaskRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: "lineage-child", TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	if scope.TurnID == uuid.Nil() {
		req.TurnID = nil
		req.RunGeneration = nil
		req.ActorSpeculativeInputSequence = nil
	}

	var invoked workerapi.InvokeChildTaskResponse
	f.workerCall(t, f.server.workerInvokeChildTask, req, &invoked)
	if invoked.OpenedWait == nil || invoked.Failed != nil {
		t.Fatalf("child invoke=%+v", invoked)
	}
	f.suspendWait(t, uuid.MustParse(req.RunWaitID), capture)
	parentID := f.runID
	if err := f.Pool.QueryRow(t.Context(), `SELECT child_run_id FROM run_waits WHERE id=$1`, uuid.MustParse(req.RunWaitID)).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	content := capture
	var admittedDescendantWriter, parkedChildWriter int64
	cancelChild := func() {
		canceler, err := run.NewCanceler(f.Pool)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.runID}); err != nil || !result.Changed {
			t.Fatalf("cancel child: %+v %v", result, err)
		}
	}
	if outcome == "queued cancellation" {
		cancelChild()
	} else {
		f.placeAndClaim(t)
		f.workerCall(t, f.server.workerStart, workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}, nil)
		f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "task", EntrypointDeclaredID: "test-task"}, nil)
		if outcome == "descendant cancellation" {
			cancelChildWithAdmittedDescendant(t, f, parentID, capture)
			admittedDescendantWriter = f.claim.workspace.WriterGeneration
			if err := f.Pool.QueryRow(t.Context(), `SELECT child_writer_generation FROM run_waits WHERE id=$1`, uuid.MustParse(req.RunWaitID)).Scan(&parkedChildWriter); err != nil {
				t.Fatal(err)
			}
		} else {

			if strings.HasPrefix(outcome, "nested ") {
				nestedOutcome := "success"
				if outcome == "nested descendant cancellation" || outcome == "nested retry cancellation" {
					nestedOutcome = strings.TrimPrefix(outcome, "nested ")
				}
				checkpointChildAndResume(t, f, session.TurnScope{}, capture, nestedOutcome)
				if nestedOutcome == "descendant cancellation" || nestedOutcome == "retry cancellation" {
					outcome = "success"
				} else {
					outcome = strings.TrimPrefix(outcome, "nested ")
				}
			}

			parked := strings.HasPrefix(outcome, "parked ")
			if parked {
				reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
				registration.TurnID = pgtype.UUID{}
				registration.RunGeneration = pgtype.Int8{}
				registration.ActorSpeculativeInputSequence = pgtype.Int8{}
				if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
					t.Fatal(err)
				}
				f.suspendWait(t, registration.WaitID, capture)
				if outcome != "parked cancellation" {
					if _, err := f.server.db.CompleteToken(t.Context(), db.CompleteTokenParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(registration.TokenID), CompletionFingerprint: make([]byte, 32), Result: []byte(`true`), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}); err != nil {
						t.Fatal(err)
					}
					if result, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, registration.TokenID, 1); err != nil || result.Resolved != 1 {
						t.Fatalf("parked readiness: %+v %v", result, err)
					}
				}

			}

			if outcome == "parked preparation failure" {
				for i := 0; i < 8; i++ {
					tx, err := f.Pool.Begin(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					graph, err := run.LockOwnedFinalization(t.Context(), tx, run.OwnedFinalizationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: f.runID})
					if err != nil {
						tx.Rollback(t.Context())
						t.Fatal(err)
					}
					exhausted, err := graph.ChargeRuntimePreparationFailure(t.Context())
					if err != nil || exhausted != (i == 7) {
						tx.Rollback(t.Context())
						t.Fatalf("preparation attempt %d exhausted=%v: %v", i, exhausted, err)
					}
					if err = tx.Commit(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
			} else if outcome == "retry cancellation" || outcome == "retry exhaustion" {
				content = finishCheckpointChild(t, f, capture, "failure")
				var status string
				var attempt int32
				var lease pgtype.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT status,current_attempt_number,current_run_lease_id FROM runs WHERE id=$1`, f.runID).Scan(&status, &attempt, &lease); err != nil {
					t.Fatal(err)
				}
				if status != "retry_delayed" || attempt != 2 || lease.Valid {
					t.Fatalf("retry was not scheduled without a lease: status=%s attempt=%d lease=%v", status, attempt, lease)
				}
				if outcome == "retry cancellation" {
					cancelChild()
				} else {
					f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{
						OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID),
						CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()},
					}, nil)
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET retry_at=transaction_timestamp()-interval '1 second' WHERE id=$1`, f.runID)
					if readied, err := f.server.db.ReadyRunRetries(t.Context(), 1); err != nil || len(readied) != 1 {
						t.Fatalf("ready retry: %+v %v", readied, err)
					}
					f.placeAndClaim(t)
					f.workerCall(t, f.server.workerStart, workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}, nil)
					f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "task", EntrypointDeclaredID: "test-task"}, nil)
					content = finishCheckpointChild(t, f, capture, "failure")
				}
			} else if outcome == "cancellation" || parked {
				cancelChild()
			} else {
				content = finishCheckpointChild(t, f, capture, outcome)
			}
			if !parked {
				f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{
					OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID),
					CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()},
				}, nil)
			}
		}
	}
	var held bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT dispatch_hold_id IS NOT NULL FROM sessions WHERE id=$1`, f.sessionID).Scan(&held); err != nil || held {
		t.Fatalf("child-only outcome held parent=%v: %v", held, err)
	}
	t.Logf("restoring parent %s after child %s", parentID, outcome)

	f.runID = parentID
	if outcome == "retry exhaustion" {
		var reclaimedAt time.Time
		var reclaimEvidence []byte
		var revision int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT runtime.reclaimed_at,runtime.reclaim_evidence,parent.revision FROM runtime_instances runtime CROSS JOIN runs parent WHERE runtime.id=$1 AND parent.id=$2`, f.claim.runtime.ID, parentID).Scan(&reclaimedAt, &reclaimEvidence, &revision); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed',reclaimed_at=NULL,reclaim_evidence=NULL WHERE id=$1`, f.claim.runtime.ID)
		if _, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(parentID), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
			t.Fatalf("unreclaimed exhausted child placement: %v", err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reclaimed_at=$2,reclaim_evidence=$3 WHERE id=$1`, f.claim.runtime.ID, reclaimedAt, reclaimEvidence)
	}
	if outcome == "success" && scope.TurnID != uuid.Nil() {
		var childVersion, parentVersion pgtype.UUID
		var revision int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT w.resume_workspace_version_id,v.parent_version_id,r.revision FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN workspace_versions v ON v.id=w.resume_workspace_version_id WHERE w.id=$1`, uuid.MustParse(req.RunWaitID)).Scan(&childVersion, &parentVersion, &revision); err != nil {
			t.Fatal(err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_versions SET parent_version_id=(SELECT head_version_id FROM workspaces WHERE id=workspace_versions.workspace_id) WHERE id=$1`, childVersion)
		_, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(parentID), ExpectedRunRevision: revision})
		if !errors.Is(err, dispatch.ErrCandidateChanged) {
			t.Fatalf("wrong latest child output parent placement: %v", err)
		}
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE workspace_versions SET parent_version_id=$2 WHERE id=$1`, childVersion, parentVersion)
	}
	f.placeAndClaim(t)
	if outcome == "descendant cancellation" {
		f.expireRestore(t)
		f.placeAndClaim(t)
	}
	f.startClaim(t)
	if admittedDescendantWriter > 0 {
		var childWriter, resumeWriter int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT child_writer_generation,resume_writer_generation FROM run_waits WHERE id=$1`, uuid.MustParse(req.RunWaitID)).Scan(&childWriter, &resumeWriter); err != nil {
			t.Fatal(err)
		}
		if childWriter != parkedChildWriter || resumeWriter <= admittedDescendantWriter {
			t.Fatalf("child provenance overwritten or writer not advanced: child=%d want%d resume=%d descendant=%d", childWriter, parkedChildWriter, resumeWriter, admittedDescendantWriter)
		}
	}
	return content
}

func TestSessionChildHandbackCheckpointLineagePostgres(t *testing.T) {
	for _, test := range []struct {
		outcome string
		again   bool
	}{
		{"success", false}, {"success", true}, {"failure", false}, {"failure", true}, {"cancellation", false}, {"cancellation", true}, {"queued cancellation", false}, {"nested success", true}, {"nested failure", true}, {"parked cancellation", true}, {"parked ready cancellation", true}, {"parked preparation failure", true}, {"descendant cancellation", true}, {"nested descendant cancellation", true}, {"retry cancellation", true}, {"nested retry cancellation", true}, {"retry exhaustion", true},
	} {
		name := test.outcome + "/direct settlement"
		if test.again {
			name = test.outcome + "/next Token checkpoint"
		}
		t.Run(name, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			capture := f.capture(t, "parent frontier")
			f.turn(t, 1, capture, true)
			if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"sequence":2}`)}); err != nil {
				t.Fatal(err)
			}
			scope := f.receiveTurn(t, 2)
			checkpointTokenAndResume(t, f, scope, 2, capture)
			child := checkpointChildAndResume(t, f, scope, capture, test.outcome)
			if test.again {
				checkpoint := checkpointTokenAndResume(t, f, scope, 2, child)

				var head pgtype.UUID
				if err := f.Pool.QueryRow(t.Context(), `SELECT head_version_id FROM workspaces WHERE id=$1`, f.workspaceID).Scan(&head); err != nil {
					t.Fatal(err)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				corrupt := `UPDATE run_waits SET resume_workspace_version_id=base_workspace_version_id WHERE id=(SELECT prior.run_wait_id FROM run_checkpoints current JOIN run_leases source ON source.id=current.source_run_lease_id JOIN runtime_instances runtime ON runtime.id=source.runtime_instance_id JOIN run_checkpoints prior ON prior.id=runtime.restore_checkpoint_id WHERE current.id=$1)`
				if test.outcome != "success" {
					corrupt = `UPDATE run_waits SET condition_status=CASE WHEN condition_status='failed' THEN 'cancelled' ELSE 'failed' END WHERE id=(SELECT prior.run_wait_id FROM run_checkpoints current JOIN run_leases source ON source.id=current.source_run_lease_id JOIN runtime_instances runtime ON runtime.id=source.runtime_instance_id JOIN run_checkpoints prior ON prior.id=runtime.restore_checkpoint_id WHERE current.id=$1)`
				}
				if strings.HasPrefix(test.outcome, "nested ") {
					corrupt = `UPDATE runs SET base_workspace_version_id=(SELECT head_version_id FROM workspaces WHERE id=runs.workspace_id) WHERE id=(SELECT w.child_run_id FROM run_checkpoints current JOIN run_leases source ON source.id=current.source_run_lease_id JOIN runtime_instances runtime ON runtime.id=source.runtime_instance_id JOIN run_checkpoints prior ON prior.id=runtime.restore_checkpoint_id JOIN run_waits w ON w.id=prior.run_wait_id WHERE current.id=$1)`
				}

				if strings.HasPrefix(test.outcome, "parked ") {
					corrupt = `UPDATE run_attempts a SET terminal_outcome=CASE WHEN a.terminal_outcome='failed' THEN 'cancelled' ELSE 'failed' END FROM runs child WHERE child.id=a.run_id AND child.current_attempt_number=a.number AND child.id=(SELECT w.child_run_id FROM run_checkpoints current JOIN run_leases source ON source.id=current.source_run_lease_id JOIN runtime_instances runtime ON runtime.id=source.runtime_instance_id JOIN run_checkpoints prior ON prior.id=runtime.restore_checkpoint_id JOIN run_waits w ON w.id=prior.run_wait_id WHERE current.id=$1)`
				}

				if _, err = tx.Exec(t.Context(), corrupt, uuid.MustParse(checkpoint.CheckpointID)); err != nil {
					t.Fatal(err)
				}
				valid, err := db.New(tx).ActorCheckpointLineageIsValid(t.Context(), db.ActorCheckpointLineageIsValidParams{RunID: pgvalue.UUID(f.runID), AttemptNumber: 1, WorkspaceID: pgvalue.UUID(f.workspaceID), CheckpointID: pgvalue.UUID(uuid.MustParse(checkpoint.CheckpointID)), CommittedHeadVersionID: head, OwnershipGeneration: f.claim.workspace.OwnershipGeneration})
				if err != nil || valid {
					t.Fatalf("wrong child handback accepted=%v err=%v", valid, err)
				}
				if err := tx.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			f.turn(t, 2, child, false)
		})
	}
}

func finishCheckpointChild(t *testing.T, f *actorCheckpointFixture, capture testWorkspaceCapture, outcome string) testWorkspaceCapture {
	t.Helper()
	content := capture
	operation := uuid.NewV7().String()
	kind := workerapi.RunFinalizationCapture
	var began workerapi.BeginRunFinalizationResponse
	f.workerCall(t, f.server.workerBeginRunFinalization, workerapi.BeginRunFinalizationRequest{Lease: f.fence(), ProgramQuiesced: workerapi.RunQuiescenceProof{RunID: f.runID.String(), AttemptNumber: f.claim.attempt.Number, RunLeaseID: f.fence().ID}, OperationID: operation, Kind: kind}, &began)
	a := f.claim
	assignment, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{run: a.run, attempt: a.attempt, runtime: a.runtime, runLease: a.runLease, workspace: a.workspace, workspaceMount: a.workspaceMount, workspaceLease: a.workspaceLease})
	if err != nil {
		t.Fatal(err)
	}
	assignment.ExpiresAt = began.ExpiresAt
	proof := validTaskWorkspaceCapture(t, assignment)
	proof.Receipt.OperationID = operation
	if outcome == "success" {
		content = f.capture(t, "child handback")
		setCaptureFingerprint(t, proof)
		f.registerFinalizationDisk(t, proof, content.Artifact.Digest)
		f.workerCall(t, f.server.workerCompleteTask, workerapi.CompleteTaskRequest{Lease: f.fence(), Outcome: workerapi.TaskOutcome{Succeeded: &workerapi.TaskSucceeded{Output: json.RawMessage(`true`)}}, Workspace: workerapi.TaskWorkspaceProof{Captured: proof}}, nil)
	} else {
		content = f.capture(t, "failed child retained changes")
		setCaptureFingerprint(t, proof)
		f.registerFinalizationDisk(t, proof, content.Artifact.Digest)
		f.workerCall(t, f.server.workerCompleteTask, workerapi.CompleteTaskRequest{Lease: f.fence(), Outcome: workerapi.TaskOutcome{Failed: &workerapi.TaskFailure{Message: "child failed"}}, Workspace: workerapi.TaskWorkspaceProof{Captured: proof}}, nil)
	}
	return content
}

func cancelChildWithAdmittedDescendant(t *testing.T, f *actorCheckpointFixture, waitingParent uuid.UUID, capture testWorkspaceCapture) {
	t.Helper()
	cancelledChild := f.runID
	target, _ := json.Marshal(map[string]string{"id": f.workspaceID.String()})
	request := workerapi.InvokeChildTaskRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: "admitted-descendant"}
	var response workerapi.InvokeChildTaskResponse
	f.workerCall(t, f.server.workerInvokeChildTask, request, &response)
	if response.OpenedWait == nil || response.Failed != nil {
		t.Fatalf("descendant invoke: %+v", response)
	}
	f.suspendWait(t, uuid.MustParse(request.RunWaitID), capture)
	if err := f.Pool.QueryRow(t.Context(), `SELECT child_run_id FROM run_waits WHERE id=$1`, uuid.MustParse(request.RunWaitID)).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	f.placeAndClaim(t)
	f.workerCall(t, f.server.workerStart, workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}, nil)
	f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "task", EntrypointDeclaredID: "test-task"}, nil)
	canceler, err := run.NewCanceler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	result, err := canceler.Cancel(t.Context(), run.CancellationRequest{OrgID: f.OrgID, ProjectID: f.ProjectID, EnvironmentID: f.EnvironmentID, RunID: cancelledChild})
	if err != nil || result.CancelledRuns != 2 {
		t.Fatalf("cancel admitted descendant graph: %+v %v", result, err)
	}
	var revision int64
	if err = f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, waitingParent).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(waitingParent), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("admitted descendant before cleanup placement: %v", err)
	}
	f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID), CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()}}, nil)
	var reclaimedAt time.Time
	var reclaimEvidence []byte
	if err = f.Pool.QueryRow(t.Context(), `SELECT reclaimed_at,reclaim_evidence FROM runtime_instances WHERE id=$1`, f.claim.runtime.ID).Scan(&reclaimedAt, &reclaimEvidence); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='lost',reclaimed_at=NULL,reclaim_evidence=NULL WHERE id=$1`, f.claim.runtime.ID)
	if _, err = f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(waitingParent), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("unreclaimed descendant writer placement: %v", err)
	}
	// Physical reclaim is the authority even when the historical observation is
	// lost; restore the actual Worker cleanup receipt without changing that observation.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reclaimed_at=$2,reclaim_evidence=$3 WHERE id=$1`, f.claim.runtime.ID, reclaimedAt, reclaimEvidence)

	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET parent_owns_lifecycle=false WHERE id=$1`, f.runID)
	if _, err = f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(waitingParent), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("unowned descendant latest writer placement: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET parent_owns_lifecycle=true WHERE id=$1`, f.runID)

}
