package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func turnCommand(f *actorExecutionFixture, s session.TurnScope) workerapi.TurnExecutionRequest {
	return workerapi.TurnExecutionRequest{Lease: f.fence(), CorrelationID: uuid.NewV7().String(), TurnID: s.TurnID.String(), RunGeneration: s.RunGeneration}
}
func readyMessages(t *testing.T, f *actorExecutionFixture, s session.TurnScope) {
	t.Helper()
	var result workerapi.TurnCommandResponse
	f.workerCall(t, f.server.workerTurnMessagesReady, turnCommand(f, s), &result)
	if !result.Accepted || result.Failed != nil {
		t.Fatalf("readiness: %+v", result)
	}
}
func admitMessage(t *testing.T, f *actorExecutionFixture, key string) session.AdmissionReceipt {
	t.Helper()
	r, err := f.server.applySessionAdmission(t.Context(), session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.SendMessageOrEnqueue, Data: json.RawMessage(`{"text":"steer"}`), IdempotencyKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func claimMessage(t *testing.T, f *actorExecutionFixture, s session.TurnScope) workerapi.TurnMessageDelivery {
	t.Helper()
	var r workerapi.ClaimTurnMessageResponse
	f.workerCall(t, f.server.workerClaimTurnMessage, workerapi.ClaimTurnMessageRequest{TurnExecutionRequest: turnCommand(f, s), DeliveryID: uuid.NewV7().String()}, &r)
	if r.Delivery == nil || r.Failed != nil {
		t.Fatalf("claim: %+v", r)
	}
	return *r.Delivery
}
func finishDelivery(t *testing.T, f *actorExecutionFixture, s session.TurnScope, d workerapi.TurnMessageDelivery, status, code string) {
	t.Helper()
	var r workerapi.TurnCommandResponse
	f.workerCall(t, f.server.workerCompleteTurnMessage, workerapi.CompleteTurnMessageRequest{TurnExecutionRequest: turnCommand(f, s), MessageID: d.MessageID, DeliveryID: d.DeliveryID, Status: status, Code: code}, &r)
	if !r.Accepted || r.Failed != nil {
		t.Fatalf("message completion: %+v", r)
	}
}

func TestSessionMessageSettlementBarrierPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	request := session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.SendMessageOrEnqueue, Data: json.RawMessage(`null`), IdempotencyKey: "not-ready"}
	first, err := f.server.applySessionAdmission(t.Context(), request)
	if err != nil || first.Kind != "messaged" {
		t.Fatalf("pre-handler admission: %+v %v", first, err)
	}
	view, err := session.GetTurn(t.Context(), f.server.db, request.Target, scope.TurnID)
	if err != nil || !view.AcceptsMessages {
		t.Fatalf("pre-handler read view: %+v %v", view, err)
	}
	readyMessages(t, f, scope)
	repeated, err := f.server.applySessionAdmission(t.Context(), request)
	if err != nil || repeated.ID != first.ID || *repeated.MessageID != *first.MessageID {
		t.Fatalf("accepted receipt changed after readiness: %+v %v", repeated, err)
	}
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
	commit := turnCommitRequest(t, f, scope) // Begins settlement before committing the result.
	request.IdempotencyKey = "settling"
	var operation *session.OperationError
	if _, err = f.server.applySessionAdmission(t.Context(), request); !errors.As(err, &operation) || operation.Code != "turn_settling" {
		t.Fatalf("admission after settlement cutoff: %v", err)
	}
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
func actorTokenWait(t *testing.T, f *actorExecutionFixture, s session.TurnScope) (*token.WaitReconciler, token.WaitRegistration) {
	t.Helper()
	tokenID := uuid.NewV7()
	if _, err := f.server.db.CreateToken(t.Context(), db.CreateTokenParams{ID: pgvalue.UUID(tokenID), OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), ExpiresAt: pgvalue.Timestamptz(time.Now().Add(time.Hour)), CallbackSecretFingerprint: make([]byte, 32), Metadata: []byte(`{}`), Tags: []string{}}); err != nil {
		t.Fatal(err)
	}
	reconciler, err := token.NewWaitReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	return reconciler, token.WaitRegistration{TokenID: tokenID, WaitID: uuid.NewV7(), RunLeaseID: pgvalue.MustUUIDValue(f.claim.runLease.ID), LeaseSequence: f.fence().LeaseSequence, WorkerGroupID: f.worker.GroupID, WorkerHostID: f.worker.HostID, WorkerEpoch: f.worker.Epoch, RequestFingerprint: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ActorSpeculativeInputSequence: pgtype.Int8{Int64: 1, Valid: true}, TurnID: pgvalue.UUID(s.TurnID), RunGeneration: pgtype.Int8{Int64: s.RunGeneration, Valid: true}}
}
func TestSessionMessageWithoutHandlerRejectedAtSettlementPostgres(t *testing.T) {
	f := newActorExecutionFixture(t, json.RawMessage(`{"sequence":1}`), true)
	scope := f.receiveTurn(t, 1)
	accepted := admitMessage(t, f, "no-handler")
	turnCommitRequest(t, f, scope)
	var status string
	var outcome []byte
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,outcome FROM session_messages WHERE id=$1`, *accepted.MessageID).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || !bytes.Contains(outcome, []byte("turn_settling")) {
		t.Fatalf("unhandled input lost: %s %s", status, outcome)
	}
}
