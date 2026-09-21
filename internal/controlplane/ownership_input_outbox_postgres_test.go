package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestOwnershipQueuedTurnRetainsReceiptAndOutboxAcrossCheckpoint(t *testing.T) {
	ctx := t.Context()
	f := newActorCheckpointFixture(t)
	f.turn(t, 1)
	waitID := uuid.NewV7()
	seq := int64(1)
	params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: seq})
	f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &seq}, nil)
	dbtest.MustExec(t, ctx, f.Pool, "UPDATE run_waits SET checkpoint_due_at=now()-interval '1 second' WHERE id=$1", waitID)
	var beforeVersion int64
	if err := f.Pool.QueryRow(ctx, "SELECT revision FROM runs WHERE id=$1", f.runID).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	receipt, err := f.server.applySessionAdmission(ctx, session.AdmissionRequest{Target: session.Target{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID}, Mode: session.EnqueueOnly, Data: []byte(`{"message":"durable"}`), IdempotencyKey: "ownership-checkpoint"})
	if err != nil {
		t.Fatal(err)
	}
	turnID := receipt.TurnID
	parsed, err := parseRunLeaseFence(f.fence())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.server.requestWorkerRunWaitCheckpoint(ctx, f.worker, f.fence(), parsed, waitID); err != nil {
		t.Fatal(err)
	}
	q := db.New(f.Pool)
	w, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: pgvalue.UUID(waitID), RunID: pgvalue.UUID(f.runID), AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	var status db.RunStatus
	var claims, records, outboxes int
	if err := f.Pool.QueryRow(ctx, "SELECT revision,status FROM runs WHERE id=$1", f.runID).Scan(&version, &status); err != nil {
		t.Fatal(err)
	}
	if version != beforeVersion || status != db.RunStatusWaiting || w.ConditionStatus != db.WaitStatusPending || w.SuspensionStatus != db.RunWaitStatusCheckpointing {
		t.Fatalf("partial inline transition run=%s/%d wait=%+v", status, version, w)
	}
	if err := f.Pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM session_turns WHERE id=$1),
 (SELECT count(*) FROM idempotency_claims WHERE id=$2 AND status='completed'),
 (SELECT count(*) FROM control_outbox WHERE topic='session.input.reconcile' AND payload->>'turnId'=$3)`, turnID, receipt.ID, turnID.String()).Scan(&records, &claims, &outboxes); err != nil {
		t.Fatal(err)
	}
	if records != 1 || claims != 1 || outboxes != 1 {
		t.Fatalf("durability records=%d claims=%d outbox=%d", records, claims, outboxes)
	}
	reconciler, err := session.NewReconciler(f.Pool)
	if err != nil {
		t.Fatal(err)
	}
	var delivered atomic.Int32
	worker, err := session.NewDeliveryWorker(nil, q, func(ctx context.Context, e, s, r uuid.UUID) (bool, error) {
		deferred, err := reconciler.ReconcileInput(ctx, e, s, r)
		if r == turnID && err == nil {
			delivered.Add(1)
		}
		return deferred, err
	}, reconciler.ReconcileClose)
	if err != nil {
		t.Fatal(err)
	}
	deliveryCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(deliveryCtx) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var state string
		if err := f.Pool.QueryRow(ctx, "SELECT status FROM control_outbox WHERE topic='session.input.reconcile' AND payload->>'turnId'=$1", turnID.String()).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("outbox delivery did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if delivered.Load() != 1 {
		t.Fatalf("deliveries=%d", delivered.Load())
	}
	completed, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: w.ID, RunID: w.RunID, AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if completed.ConditionStatus != db.WaitStatusCompleted || completed.CompletedTurnID != pgvalue.UUID(turnID) {
		t.Fatalf("eventual completion=%+v", completed)
	}
	if _, err := reconciler.ReconcileInput(ctx, f.EnvironmentID, f.sessionID, turnID); err != nil {
		t.Fatal(err)
	}
	repeated, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: w.ID, RunID: w.RunID, AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.UpdatedAt != completed.UpdatedAt || repeated.ExpectedRunRevision != completed.ExpectedRunRevision {
		t.Fatal("repeated delivery changed completed Wait")
	}
}
