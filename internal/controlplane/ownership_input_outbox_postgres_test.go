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
	"github.com/jackc/pgx/v5/pgxpool"
)

type ownershipAppendStore struct {
	db.Querier
	pool        *pgxpool.Pool
	afterLocate func(db.RunWait) error
}

func (s ownershipAppendStore) BeginQuerier(ctx context.Context) (db.Querier, transaction, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, err
	}
	return ownershipAppendQueries{Querier: db.New(tx), afterLocate: s.afterLocate}, tx, nil
}

type ownershipAppendQueries struct {
	db.Querier
	afterLocate func(db.RunWait) error
}

func (q ownershipAppendQueries) LocatePendingActorInputRunWait(ctx context.Context, p db.LocatePendingActorInputRunWaitParams) (db.RunWait, error) {
	w, err := q.Querier.LocatePendingActorInputRunWait(ctx, p)
	if err == nil {
		err = q.afterLocate(w)
	}
	return w, err
}

func TestOwnershipAdmittedInputStaleLocatorKeepsClaimAndOutbox(t *testing.T) {
	ctx := t.Context()
	f := newActorCheckpointFixture(t)
	waitID := uuid.NewV7()
	seq := int64(1)
	params, _ := json.Marshal(workerActorInputWaitParams{SessionID: f.sessionID.String(), AfterInputSequence: seq})
	f.workerCall(t, f.server.workerCreateRunWait, workerapi.CreateRunWaitRequest{CorrelationID: uuid.NewV7().String(), Lease: f.fence(), RunWaitID: waitID.String(), ResumeAttachID: uuid.NewV7().String(), Kind: "actor_input", Params: params, ActorSpeculativeInputSequence: &seq}, nil)
	dbtest.MustExec(t, ctx, f.Pool, "UPDATE run_waits SET checkpoint_due_at=now()-interval '1 second' WHERE id=$1", waitID)
	var beforeVersion int64
	if err := f.Pool.QueryRow(ctx, "SELECT state_version FROM runs WHERE id=$1", f.runID).Scan(&beforeVersion); err != nil {
		t.Fatal(err)
	}
	// Exercise the repository no-row boundary between locator and completion with
	// a separately committed checkpoint request. This does not claim the full
	// worker HTTP owner can bypass the append transaction's Actor lock.
	f.server.db = ownershipAppendStore{Querier: db.New(f.Pool), pool: f.Pool, afterLocate: func(w db.RunWait) error {
		tx, err := f.Pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		q := db.New(tx)
		checkpointID := pgvalue.UUID(uuid.NewV7())
		if _, err := q.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{ID: checkpointID, RunID: w.RunID, AttemptNumber: w.AttemptNumber, RunWaitID: w.ID, SourceRunLeaseID: w.CurrentRunLeaseID, SourceWorkspaceLeaseID: f.claim.workspaceLease.ID, WorkspaceID: w.WorkspaceID, BaseWorkspaceVersionID: f.claim.workspaceLease.BaseVersionID, ActorSpeculativeInputSequence: w.ActorSpeculativeInputSequence, RestoreManifest: []byte("{}")}); err != nil {
			return err
		}
		if _, err := q.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{ID: w.ID, RunID: w.RunID, AttemptNumber: w.AttemptNumber, CurrentRunLeaseID: w.CurrentRunLeaseID, SuspendCheckpointID: checkpointID}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}}
	recordID := uuid.NewV7()
	record, err := f.server.appendActorInput(ctx, appendActorInputRequest{EnvironmentID: f.EnvironmentID, SessionID: f.sessionID, RecordID: recordID, Data: []byte(`{"message":"durable"}`), IdempotencyKey: "ownership-stale-locator"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 2 {
		t.Fatalf("record=%+v", record)
	}
	q := db.New(f.Pool)
	w, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: pgvalue.UUID(waitID), RunID: pgvalue.UUID(f.runID), AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	var version int64
	var status db.RunStatus
	var claims, records, outboxes int
	if err := f.Pool.QueryRow(ctx, "SELECT state_version,status FROM runs WHERE id=$1", f.runID).Scan(&version, &status); err != nil {
		t.Fatal(err)
	}
	if version != beforeVersion || status != db.RunStatusWaiting || w.ConditionState != db.WaitStatePending || w.SuspensionState != db.RunWaitStateCheckpointing {
		t.Fatalf("partial inline transition run=%s/%d wait=%+v", status, version, w)
	}
	if err := f.Pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM session_records WHERE id=$1),
 (SELECT count(*) FROM idempotency_claims WHERE id=$2 AND state='completed'),
 (SELECT count(*) FROM control_outbox WHERE topic='session.input.reconcile' AND payload->>'recordId'=$3)`, record.ID, record.ClaimID, recordID.String()).Scan(&records, &claims, &outboxes); err != nil {
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
		if r == recordID && err == nil {
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
		if err := f.Pool.QueryRow(ctx, "SELECT state FROM control_outbox WHERE topic='session.input.reconcile' AND payload->>'recordId'=$1", recordID.String()).Scan(&state); err != nil {
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
	if completed.ConditionState != db.WaitStateCompleted || completed.CompletedActorRecordID != record.ID {
		t.Fatalf("eventual completion=%+v", completed)
	}
	if _, err := reconciler.ReconcileInput(ctx, f.EnvironmentID, f.sessionID, recordID); err != nil {
		t.Fatal(err)
	}
	repeated, err := q.GetRunWait(ctx, db.GetRunWaitParams{ID: w.ID, RunID: w.RunID, AttemptNumber: 1})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.UpdatedAt != completed.UpdatedAt || repeated.ExpectedRunStateVersion != completed.ExpectedRunStateVersion {
		t.Fatal("repeated delivery changed completed Wait")
	}
}
