package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"uuid"
)

func turnCommand(f *actorCheckpointFixture, s session.TurnScope) workerapi.TurnExecutionRequest {
	return workerapi.TurnExecutionRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: s.TurnID.String(), RunGeneration: s.RunGeneration}
}
func readyMessages(t *testing.T, f *actorCheckpointFixture, s session.TurnScope) {
	t.Helper()
	var result workerapi.TurnCommandResponse
	f.workerCall(t, f.server.workerTurnMessagesReady, turnCommand(f, s), &result)
	if !result.Accepted || result.Failed != nil {
		t.Fatalf("readiness: %+v", result)
	}
}
func admitMessage(t *testing.T, f *actorCheckpointFixture, key string) session.AdmissionReceipt {
	t.Helper()
	r, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.SendMessageOrEnqueue, Data: json.RawMessage(`{"text":"steer"}`), IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func claimMessage(t *testing.T, f *actorCheckpointFixture, s session.TurnScope) workerapi.TurnMessageDelivery {
	t.Helper()
	var r workerapi.ClaimTurnMessageResponse
	f.workerCall(t, f.server.workerClaimTurnMessage, workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: turnCommand(f, s), DeliveryID: uuid.NewV7().String()}, &r)
	if r.Delivery == nil || r.Failed != nil {
		t.Fatalf("claim: %+v", r)
	}
	return *r.Delivery
}
func finishDelivery(t *testing.T, f *actorCheckpointFixture, s session.TurnScope, d workerapi.TurnMessageDelivery, status, code string) {
	t.Helper()
	var r workerapi.TurnCommandResponse
	f.workerCall(t, f.server.workerCompleteTurnMessage, workerapi.CompleteTurnMessageRequest{TurnExecutionRequest: turnCommand(f, s), MessageID: d.MessageID, DeliveryID: d.DeliveryID, Status: status, Code: code}, &r)
	if !r.Accepted || r.Failed != nil {
		t.Fatalf("message completion: %+v", r)
	}
}

func TestSessionMessageSettlementBarrierPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	request := session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.SendMessageOrEnqueue, Data: json.RawMessage(`null`), IdempotencyKey: "not-ready"}
	_, err := f.server.applySessionAdmission(t.Context(), request)
	var operation *session.OperationError
	if !errors.As(err, &operation) || operation.Code != "turn_not_ready" {
		t.Fatalf("unready send: %v", err)
	}
	readyMessages(t, f, scope)
	if _, err = f.server.applySessionAdmission(t.Context(), request); !errors.As(err, &operation) || operation.Code != "turn_not_ready" {
		t.Fatalf("rejected receipt changed after readiness: %v", err)
	}
	first := admitMessage(t, f, "message-1")
	second := admitMessage(t, f, "message-2")
	queued, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: request.Target, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"next":true}`)})
	if err != nil {
		t.Fatal(err)
	}
	if queued.Kind != "enqueued" || first.Kind != "messaged" {
		t.Fatalf("routing: %+v %+v", first, queued)
	}
	delivered := claimMessage(t, f, scope)
	if delivered.MessageID != first.MessageID.String() {
		t.Fatalf("delivery order: %+v", delivered)
	}
	commit := turnCommitRequest(t, f, scope, f.capture(t, "message result")) // Begins settlement before the physical commit.
	var secondStatus string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM session_messages WHERE id=$1`, *second.MessageID).Scan(&secondStatus); err != nil || secondStatus != "rejected" {
		t.Fatalf("queued callback at barrier: %s %v", secondStatus, err)
	}
	root := outputRequest(f, scope)
	root.IdempotencyKey = "root-after-barrier"
	if r := appendOutput(t, f, root); r.Failed == nil || r.Failed.Code != "turn_unsettled" {
		t.Fatalf("root output after barrier: %+v", r)
	}
	cleanup := root
	cleanup.IdempotencyKey = "admitted-callback-cleanup"
	cleanup.MessageDeliveryID = &delivered.DeliveryID
	if r := appendOutput(t, f, cleanup); r.Failed != nil || r.Completed == nil {
		t.Fatalf("admitted cleanup: %+v", r)
	}
	parsed, err := parseActorTurnCommitRequest(commit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.commitActorTurn(t.Context(), f.worker, commit, parsed); !errors.Is(err, errStaleActorTurnCommit) {
		t.Fatalf("settled with active delivery: %v", err)
	}
	finishDelivery(t, f, scope, delivered, "handled", "")
	if r := appendOutput(t, f, cleanup); r.Failed == nil || r.Failed.Code != "stale_execution" {
		t.Fatalf("historical cleanup receipt admitted after callback lifetime: %+v", r)
	}
	if _, err = f.server.commitActorTurn(t.Context(), f.worker, commit, parsed); err != nil {
		t.Fatal(err)
	}
	next := f.receiveTurn(t, 2)
	if next.TurnID != queued.TurnID {
		t.Fatalf("queued identity changed: %+v %+v", next, queued)
	}
}
func TestSessionUnknownMessageRetainsActiveTurnPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	scope := f.receiveTurn(t, 1)
	readyMessages(t, f, scope)
	admitMessage(t, f, "unknown")
	delivery := claimMessage(t, f, scope)
	finishDelivery(t, f, scope, delivery, "unknown", "handler_failed")
	var status, reason string
	var active uuid.UUID
	var cursor int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT m.status,s.dispatch_hold_reason,s.active_turn_id,s.committed_input_sequence FROM sessions s JOIN session_messages m ON m.session_id=s.id WHERE s.id=$1`, f.sessionID).Scan(&status, &reason, &active, &cursor); err != nil {
		t.Fatal(err)
	}
	if status != "unknown" || reason != "recovery_required" || active != scope.TurnID || cursor != 0 {
		t.Fatalf("unknown outcome: %s %s %s %d", status, reason, active, cursor)
	}
	var r workerapi.ClaimTurnMessageResponse
	f.workerCall(t, f.server.workerClaimTurnMessage, workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: turnCommand(f, scope), DeliveryID: delivery.DeliveryID}, &r)
	if r.Failed == nil || r.Failed.Code != "turn_stopping" || r.Delivery != nil {
		t.Fatalf("unknown delivery replay: %+v", r)
	}
}
func actorTokenWait(t *testing.T, f *actorCheckpointFixture, s session.TurnScope) (*token.WaitReconciler, token.WaitRegistration) {
	t.Helper()
	tokenID := uuid.NewV7()
	if _, err := f.server.db.CreateToken(t.Context(), db.CreateTokenParams{ID: pgvalue.UUID(tokenID), OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)), CallbackSecretFingerprint: make([]byte, 32), Metadata: []byte(`{}`), Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	reconciler, err := token.NewWaitReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, token.WaitRegistration{TokenID: tokenID, WaitID: uuid.NewV7(), ResumeAttachID: uuid.NewV7(), RunLeaseID: pgvalue.MustUUIDValue(f.claim.runLease.ID), LeaseSequence: f.fence().LeaseSequence, WorkerGroupID: f.worker.WorkerGroupID, WorkerInstanceID: f.worker.WorkerInstanceID, WorkerEpoch: f.worker.WorkerEpoch, RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActorSpeculativeInputSequence: pgtype.Int8{Int64: 1, Valid: true}, TurnID: pgvalue.UUID(s.TurnID), RunGeneration: pgtype.Int8{Int64: s.RunGeneration, Valid: true}, CheckpointDueAt: pgvalue.Timestamptz(time.Now().Add(-time.Minute))}
}
func TestSessionTokenWaitStopOrderingPostgres(t *testing.T) {
	t.Run("outside Turn cannot speculate queued input", func(t *testing.T) {
		f := newActorCheckpointFixture(t)
		reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
		registration.TurnID = pgtype.UUID{}
		registration.RunGeneration = pgtype.Int8{}
		if _, err := reconciler.RegisterWait(t.Context(), registration); !errors.Is(err, token.ErrWaitAuthority) {
			t.Fatalf("unadmitted input became speculative cursor: %v", err)
		}
		registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 0, Valid: true}
		if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
			t.Fatalf("committed outside-Turn position rejected: %v", err)
		}
		var active pgtype.UUID
		var cursor int64
		if err := f.Pool.QueryRow(t.Context(), `SELECT active_turn_id,committed_input_sequence FROM sessions WHERE id=$1`, f.sessionID).Scan(&active, &cursor); err != nil || active.Valid || cursor != 0 {
			t.Fatalf("wait fabricated input admission: %v %d %v", active, cursor, err)
		}
	})
	t.Run("stop before registration", func(t *testing.T) {
		f := newActorCheckpointFixture(t)
		scope := f.receiveTurn(t, 1)
		reconciler, registration := actorTokenWait(t, f, scope)
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		graph, err := lockSessionControlGraph(t.Context(), &txWork{q: db.New(tx), tx: tx}, session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = session.InterruptTurn(t.Context(), db.New(tx), f.EnvironmentID, f.sessionID, scope.TurnID, "before-wait", graph); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := reconciler.RegisterWait(t.Context(), registration); done <- err }()
		if err = tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err = <-done; !errors.Is(err, token.ErrWaitAuthority) {
			t.Fatalf("registration after stop: %v", err)
		}
		var waits int
		var tokenStatus string
		if err = f.Pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM run_waits WHERE id=$1),(SELECT status FROM tokens WHERE id=$2)`, registration.WaitID, registration.TokenID).Scan(&waits, &tokenStatus); err != nil {
			t.Fatal(err)
		}
		if waits != 0 || tokenStatus != "pending" {
			t.Fatalf("stop changed shared Token: waits=%d status=%s", waits, tokenStatus)
		}
	})
	t.Run("registration before stop", func(t *testing.T) {
		f := newActorCheckpointFixture(t)
		scope := f.receiveTurn(t, 1)
		readyMessages(t, f, scope)
		reconciler, registration := actorTokenWait(t, f, scope)
		if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
			t.Fatal(err)
		}
		var bound uuid.UUID
		var gen int64
		var ready bool
		if err := f.Pool.QueryRow(t.Context(), `SELECT w.turn_id,w.turn_run_generation,t.ready_run_lease_id IS NOT NULL FROM run_waits w JOIN session_turns t ON t.id=w.turn_id WHERE w.id=$1`, registration.WaitID).Scan(&bound, &gen, &ready); err != nil {
			t.Fatal(err)
		}
		if bound != scope.TurnID || gen != scope.RunGeneration || ready {
			t.Fatalf("wait binding: %s %d ready=%v", bound, gen, ready)
		}
		if _, err := interruptTurn(t.Context(), f, scope, "during-wait"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.server.db.CompleteToken(t.Context(), db.CompleteTokenParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(registration.TokenID), CompletionFingerprint: make([]byte, 32), Result: []byte(`{"approved":true}`), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}); err != nil {
			t.Fatal(err)
		}
		batch, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, registration.TokenID, 1)
		if err != nil || batch.Examined != 0 {
			t.Fatalf("revoked wait consumed Token: %+v %v", batch, err)
		}
		if _, err = reconciler.RegisterWait(t.Context(), registration); !errors.Is(err, token.ErrWaitAuthority) {
			t.Fatalf("stopped registration replay: %v", err)
		}
		parsed, _ := parseRunLeaseFence(f.fence())
		if _, err = f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), parsed, registration.WaitID); err == nil {
			t.Fatal("stopped wait checkpoint admitted")
		}
		raw, _ := json.Marshal(workerapi.RunWaitPollRequest{Lease: f.fence(), RunWaitID: registration.WaitID.String()})
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
		request = request.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
		response := httptest.NewRecorder()
		f.server.workerPollRunWait(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("revoked poll %d: %s", response.Code, response.Body.String())
		}
		var revoked bool
		var condition string
		if err = f.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_id IS NOT NULL,w.condition_status FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN sessions s ON s.id=r.session_id WHERE w.id=$1`, registration.WaitID).Scan(&revoked, &condition); err != nil {
			t.Fatal(err)
		}
		if !revoked || condition != "pending" {
			t.Fatalf("revocation changed Token condition: %v %s", revoked, condition)
		}
	})
}

func TestSessionParkedTurnInterruptRecoveryPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "retained committed head")
	committed := f.turn(t, 1, capture, true)
	queued, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"work":2}`)})
	if err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, 2)
	if scope.TurnID != queued.TurnID {
		t.Fatal("queued Turn identity changed")
	}
	readyMessages(t, f, scope)
	admitMessage(t, f, "callback-before-park")
	delivery := claimMessage(t, f, scope)
	reconciler, registration := actorTokenWait(t, f, scope)
	registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
	if _, err = reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatal(err)
	}
	f.suspendWait(t, registration.WaitID, capture)
	var lease pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_lease_id FROM runs WHERE id=$1`, f.runID).Scan(&lease); err != nil || lease.Valid {
		t.Fatalf("not parked: %v %v", lease, err)
	}
	stopped, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, IdempotencyKey: "parked-stop"}, TurnID: scope.TurnID})
	if err != nil || stopped.HoldID == nil {
		t.Fatalf("parked stop: %+v %v", stopped, err)
	}
	var status, reason string
	var held, active, head, owner uuid.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status,s.dispatch_hold_id,s.dispatch_hold_reason,s.active_turn_id,w.head_version_id,w.owner_session_id FROM sessions s JOIN runs r ON r.id=s.current_run_id JOIN workspaces w ON w.id=s.workspace_id WHERE s.id=$1`, f.sessionID).Scan(&status, &held, &reason, &active, &head, &owner); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || held != *stopped.HoldID || reason != "interrupt_requested" || active != scope.TurnID || head.String() != committed.WorkspaceVersionID || owner != f.sessionID {
		t.Fatalf("parked retirement changed authority: %s %s %s %s %s %s", status, held, reason, active, head, owner)
	}
	request := session.RecoverRequest{ResumeRequest: session.ResumeRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, IdempotencyKey: "parked-recovery"}, HoldID: held}, TurnID: &scope.TurnID, WorkspaceVersionID: head, ReconciliationRef: "test-retained-head", Disposition: "interrupted"}
	recovered, err := f.server.applySessionRecovery(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.HoldID == nil || *recovered.HoldID == held {
		t.Fatalf("recovery did not issue exact new hold: %+v", recovered)
	}
	again, err := f.server.applySessionRecovery(t.Context(), request)
	if err != nil || again.ID != recovered.ID || *again.HoldID != *recovered.HoldID {
		t.Fatalf("recovery replay: %+v %v", again, err)
	}
	var messageStatus string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM session_messages WHERE id=$1`, uuid.MustParse(delivery.MessageID)).Scan(&messageStatus); err != nil || messageStatus != "unknown" {
		t.Fatalf("recovery redelivered or lost callback uncertainty: %s %v", messageStatus, err)
	}
	var cursor int64
	var current, activeAfter pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT s.committed_input_sequence,s.active_turn_id,s.current_run_id,t.status FROM sessions s JOIN session_turns t ON t.id=$2 WHERE s.id=$1`, f.sessionID, scope.TurnID).Scan(&cursor, &activeAfter, &current, &status); err != nil {
		t.Fatal(err)
	}
	if cursor != 2 || activeAfter.Valid || current.Valid || status != "interrupted" {
		t.Fatalf("recovered state: %d %v %v %s", cursor, activeAfter, current, status)
	}
	if _, err = f.server.applySessionResume(t.Context(), session.ResumeRequest{ControlRequest: session.ControlRequest{Target: request.Target, IdempotencyKey: "parked-resume"}, HoldID: *recovered.HoldID}); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_id,dispatch_hold_id FROM sessions WHERE id=$1`, f.sessionID).Scan(&current, &activeAfter); err != nil || !current.Valid || current == pgvalue.UUID(f.runID) || activeAfter.Valid {
		t.Fatalf("explicit resume: %v %v %v", current, activeAfter, err)
	}
}

func TestSessionTokenResumeStopAuthorityPostgres(t *testing.T) {
	for _, stage := range []string{"before_start", "before_ack", "after_ack"} {
		t.Run(stage, func(t *testing.T) {
			f := newActorCheckpointFixture(t)
			capture := f.capture(t, "checkpoint head")
			f.turn(t, 1, capture, true)
			if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"work":2}`)}); err != nil {
				t.Fatal(err)
			}
			scope := f.receiveTurn(t, 2)
			reconciler, registration := actorTokenWait(t, f, scope)
			registration.ActorSpeculativeInputSequence = pgtype.Int8{Int64: 2, Valid: true}
			if _, err := reconciler.RegisterWait(t.Context(), registration); err != nil {
				t.Fatal(err)
			}
			f.suspendWait(t, registration.WaitID, capture)
			if _, err := f.server.db.CompleteToken(t.Context(), db.CompleteTokenParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(registration.TokenID), CompletionFingerprint: make([]byte, 32), Result: []byte(`{"approved":true}`), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}); err != nil {
				t.Fatal(err)
			}
			if b, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, registration.TokenID, 1); err != nil || b.Resolved != 1 {
				t.Fatalf("Token checkpoint resolution: %+v %v", b, err)
			}
			var checkpointBase, attemptBase, head uuid.UUID
			var waitKind string
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.base_workspace_version_id,a.base_workspace_version_id,w.head_version_id,rw.kind FROM run_waits rw JOIN run_checkpoints c ON c.id=rw.suspend_checkpoint_id JOIN run_attempts a ON a.run_id=rw.run_id AND a.number=rw.attempt_number JOIN workspaces w ON w.id=rw.workspace_id WHERE rw.id=$1`, registration.WaitID).Scan(&checkpointBase, &attemptBase, &head, &waitKind); err != nil {
				t.Fatal(err)
			}
			if checkpointBase != head || checkpointBase == attemptBase || waitKind != "token" {
				t.Fatalf("invalid active checkpoint fixture: %s %s %s %s", checkpointBase, attemptBase, head, waitKind)
			}
			t.Logf("active Token checkpoint uses committed head %s, attempt origin %s", checkpointBase, attemptBase)
			f.placeAndClaim(t)
			wait := f.claim.runWait
			start := workerapi.RunStartRequest{Lease: f.fence(), Restore: &workerapi.RunStartRestore{RunWaitID: pgvalue.UUIDString(wait.ID), CheckpointID: pgvalue.UUIDString(wait.SuspendCheckpointID), ResumeAttachID: pgvalue.UUIDString(wait.ResumeAttachID), ResumeRequestVersion: wait.ResumeRequestVersion}}
			ack := workerapi.RunResumeReleaseRequest{Lease: f.fence(), RunWaitID: start.Restore.RunWaitID, CheckpointID: start.Restore.CheckpointID, ResumeAttachID: start.Restore.ResumeAttachID, ResumeRequestVersion: start.Restore.ResumeRequestVersion}
			if stage != "before_start" {
				f.workerCall(t, f.server.workerStart, start, nil)
			}
			if stage == "after_ack" {
				f.workerCall(t, f.server.workerAcknowledgeRunResumeRelease, ack, nil)
			}
			if _, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, TurnID: scope.TurnID}); err != nil {
				t.Fatal(err)
			}
			var request any = ack
			handler := f.server.workerAcknowledgeRunResumeRelease
			if stage == "before_start" {
				request = start
				handler = f.server.workerStart
			}
			raw, _ := json.Marshal(request)
			r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
			r = r.WithContext(context.WithValue(t.Context(), workerContextKey{}, f.worker))
			response := httptest.NewRecorder()
			handler(response, r)
			if response.Code != http.StatusConflict {
				t.Fatalf("stopped %s admitted: %d %s", stage, response.Code, response.Body.String())
			}
			var condition string
			var revoked bool
			var ackVersion int64
			if err := f.Pool.QueryRow(t.Context(), `SELECT w.condition_status,s.dispatch_hold_id IS NOT NULL,w.resume_ack_version FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN sessions s ON s.id=r.session_id WHERE w.id=$1`, registration.WaitID).Scan(&condition, &revoked, &ackVersion); err != nil {
				t.Fatal(err)
			}
			wantAck := int64(0)
			if stage == "after_ack" {
				wantAck = wait.ResumeRequestVersion
			}
			if condition != "completed" || !revoked || ackVersion != wantAck {
				t.Fatalf("stop changed Token result or acknowledged execution: %s %v %d", condition, revoked, ackVersion)
			}
		})
	}
}

func TestSessionActiveTurnChildCallBindsWaitPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Pool.Exec(t.Context(), `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, manifest, digest[:]); err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, 1)
	readyMessages(t, f, scope)
	turnID := scope.TurnID.String()
	cursor := int64(1)
	target, _ := json.Marshal(map[string]string{"id": f.workspaceID.String()})
	request := workerapi.InvokeChildTaskRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: "active-child", TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	var response workerapi.InvokeChildTaskResponse
	f.workerCall(t, f.server.workerInvokeChildTask, request, &response)
	if response.OpenedWait == nil || response.Failed != nil {
		t.Fatalf("active child call: %+v", response)
	}
	var bound uuid.UUID
	var ready bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.turn_id,t.ready_run_lease_id IS NOT NULL FROM run_waits w JOIN session_turns t ON t.id=w.turn_id WHERE w.id=$1`, uuid.MustParse(request.RunWaitID)).Scan(&bound, &ready); err != nil || bound != scope.TurnID || ready {
		t.Fatalf("child wait binding: %s %v %v", bound, ready, err)
	}
	if _, err := f.server.applySessionInterrupt(t.Context(), session.InterruptRequest{ControlRequest: session.ControlRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}}, TurnID: scope.TurnID}); err != nil {
		t.Fatal(err)
	}
	parsed, _ := parseRunLeaseFence(f.fence())
	if _, err := f.server.requestWorkerRunWaitCheckpoint(t.Context(), f.worker, f.fence(), parsed, uuid.MustParse(request.RunWaitID)); err == nil {
		t.Fatal("stopped child wait entered checkpoint handoff")
	}
	var revoked bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT s.dispatch_hold_id IS NOT NULL FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN sessions s ON s.id=r.session_id WHERE w.id=$1`, uuid.MustParse(request.RunWaitID)).Scan(&revoked); err != nil || !revoked {
		t.Fatalf("child wait not revoked: %v %v", revoked, err)
	}
}

func TestOwnedTaskTokenWaitDoesNotInheritActorTurnPostgres(t *testing.T) {
	f := newActorCheckpointFixture(t)
	capture := f.capture(t, "Actor committed frontier")
	f.turn(t, 1, capture, true)
	if _, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: json.RawMessage(`{"work":2}`)}); err != nil {
		t.Fatal(err)
	}
	scope := f.receiveTurn(t, 2)
	manifest, digest, err := deployment.CanonicalManifestAndDigest([]byte(`{"payload":{"kind":"none"},"run":{"maxDurationMs":300000,"queue":"default","retry":{"enabled":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Pool.Exec(t.Context(), `UPDATE deployment_definitions SET manifest=$2,manifest_digest=$3 WHERE id=$1`, f.TaskDefinitionID, manifest, digest[:]); err != nil {
		t.Fatal(err)
	}
	turnID := scope.TurnID.String()
	cursor := int64(2)
	target, _ := json.Marshal(map[string]string{"id": f.workspaceID.String()})
	request := workerapi.InvokeChildTaskRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), RunWaitID: uuid.NewV7().String(), ResumeAttachID: uuid.NewV7().String(), TaskDeclaredID: "test-task", Method: "call", Workspace: target, Options: json.RawMessage(`{}`), IdempotencyKey: "generic-owned-task", TurnID: &turnID, RunGeneration: &scope.RunGeneration, ActorSpeculativeInputSequence: &cursor}
	var response workerapi.InvokeChildTaskResponse
	f.workerCall(t, f.server.workerInvokeChildTask, request, &response)
	if response.OpenedWait == nil || response.Failed != nil {
		t.Fatalf("parent call: %+v", response)
	}
	childCheckpoint := f.suspendWait(t, uuid.MustParse(request.RunWaitID), capture)
	var checkpointBase, attemptBase, committedHead, childBase pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.base_workspace_version_id,a.base_workspace_version_id,w.head_version_id,c.private_workspace_version_id FROM run_checkpoints c JOIN run_attempts a ON a.run_id=c.run_id AND a.number=c.attempt_number JOIN workspaces w ON w.id=c.workspace_id WHERE c.id=$1`, uuid.MustParse(childCheckpoint.CheckpointID)).Scan(&checkpointBase, &attemptBase, &committedHead, &childBase); err != nil {
		t.Fatal(err)
	}
	if checkpointBase != committedHead || checkpointBase == attemptBase || childBase != pgvalue.UUID(uuid.MustParse(childCheckpoint.WorkspaceVersionID)) || childBase == committedHead {
		t.Fatalf("Actor parent/Task child frontier: checkpoint=%v attempt=%v committed=%v child=%v", checkpointBase, attemptBase, committedHead, childBase)
	}
	parentID := f.runID
	if err = f.Pool.QueryRow(t.Context(), `SELECT child_run_id FROM run_waits WHERE id=$1`, uuid.MustParse(request.RunWaitID)).Scan(&f.runID); err != nil {
		t.Fatal(err)
	}
	f.placeAndClaim(t)
	f.workerCall(t, f.server.workerStart, workerapi.RunStartRequest{Lease: f.fence(), Fresh: &workerapi.RunStartFresh{}}, nil)
	f.workerCall(t, f.server.workerEnterRunEntrypoint, workerapi.RunEntrypointRequest{Lease: f.fence(), EntrypointKind: "task", EntrypointDeclaredID: "test-task"}, nil)
	reconciler, registration := actorTokenWait(t, f, session.TurnScope{})
	registration.TurnID = pgtype.UUID{}
	registration.RunGeneration = pgtype.Int8{}
	registration.ActorSpeculativeInputSequence = pgtype.Int8{}
	if _, err = reconciler.RegisterWait(t.Context(), registration); err != nil {
		t.Fatalf("generic owned Task Token registration: %v", err)
	}
	ownedCheckpoint := f.suspendWait(t, registration.WaitID, capture)

	if _, err = f.server.db.CompleteToken(t.Context(), db.CompleteTokenParams{OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: pgvalue.UUID(registration.TokenID), CompletionFingerprint: make([]byte, 32), Result: []byte(`true`), ControlOutboxID: pgvalue.UUID(uuid.NewV7())}); err != nil {
		t.Fatal(err)
	}
	if result, err := reconciler.ReconcileBatch(t.Context(), f.EnvironmentID, registration.TokenID, 1); err != nil || result.Resolved != 1 {
		t.Fatalf("owned Task Token resolution: %+v %v", result, err)
	}
	var originalBase pgtype.UUID
	var revision int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT c.base_workspace_version_id,r.revision FROM run_checkpoints c JOIN runs r ON r.id=c.run_id WHERE c.id=$1`, uuid.MustParse(ownedCheckpoint.CheckpointID)).Scan(&originalBase, &revision); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_checkpoints SET base_workspace_version_id=(SELECT head_version_id FROM workspaces WHERE id=run_checkpoints.workspace_id) WHERE id=$1`, uuid.MustParse(ownedCheckpoint.CheckpointID))
	if _, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("corrupt Task checkpoint source base placement: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_checkpoints SET base_workspace_version_id=$2 WHERE id=$1`, uuid.MustParse(ownedCheckpoint.CheckpointID), originalBase)

	f.placeAndClaim(t)
	f.expireRestore(t)
	if err := f.Pool.QueryRow(t.Context(), `SELECT revision FROM runs WHERE id=$1`, f.runID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET restore_checkpoint_id=NULL WHERE id=$1`, f.claim.runtime.ID)
	if _, err := f.placement.PlaceReadyRun(t.Context(), dispatch.ReadyRunCandidate{OrgID: pgvalue.UUID(f.OrgID), RunID: pgvalue.UUID(f.runID), ExpectedRunRevision: revision}); !errors.Is(err, dispatch.ErrCandidateChanged) {
		t.Fatalf("corrupt recovered Task restore identity placement: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET restore_checkpoint_id=$2 WHERE id=$1`, f.claim.runtime.ID, f.claim.runtime.RestoreCheckpointID)

	f.placeAndClaim(t)
	f.startClaim(t)

	checkpointTokenAndResume(t, f, session.TurnScope{}, 0, capture)

	var bound, owner, active pgtype.UUID
	var childStatus string
	if err = f.Pool.QueryRow(t.Context(), `SELECT rw.turn_id,w.owner_session_id,s.active_turn_id,r.status FROM run_waits rw JOIN runs r ON r.id=rw.run_id JOIN workspaces w ON w.id=r.workspace_id JOIN sessions s ON s.current_run_id=$2 WHERE rw.id=$1`, registration.WaitID, parentID).Scan(&bound, &owner, &active, &childStatus); err != nil {
		t.Fatal(err)
	}
	if bound.Valid || owner != pgvalue.UUID(f.sessionID) || active != pgvalue.UUID(scope.TurnID) || childStatus != "running" {
		t.Fatalf("Task inherited Actor consumption: %v %v %v %s", bound, owner, active, childStatus)
	}
	content := finishCheckpointChild(t, f, capture, "success")
	f.workerCall(t, f.server.workerStopWorkspaceMount, workerapi.WorkspaceMountStopRequest{OrgID: f.OrgID.String(), WorkspaceMountID: pgvalue.UUIDString(f.claim.workspaceMount.ID), CleanupProof: workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now()}}, nil)
	f.runID = parentID
	f.placeAndClaim(t)
	f.startClaim(t)
	f.turn(t, 2, content, false)

}
