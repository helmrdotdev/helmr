package session

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestHeldExecutionCompletesAdmittedMessage(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	actorID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	fence := run.ExecutionFence{LeaseID: pgvalue.UUID(work.LeaseID), LeaseSequence: 1, WorkerGroupID: pgvalue.UUID(runtest.WorkerGroupID), WorkerHostID: pgvalue.UUID(f.WorkerID), WorkerEpoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.claim_version,g.claim_version FROM worker_hosts h JOIN worker_groups g ON g.id=h.worker_group_id WHERE h.id=$1`, f.WorkerID).Scan(&fence.HostClaimVersion, &fence.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	transact := func(fn func(pgx.Tx) error) error {
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			return err
		}
		defer tx.Rollback(context.Background())
		if err = fn(tx); err != nil {
			return err
		}
		return tx.Commit(t.Context())
	}
	if err := transact(func(tx pgx.Tx) error { _, err := run.ClaimExecution(t.Context(), tx, fence); return err }); err != nil {
		t.Fatal(err)
	}
	if err := transact(func(tx pgx.Tx) error { _, err := run.StartExecution(t.Context(), tx, fence); return err }); err != nil {
		t.Fatal(err)
	}
	holdID, turnID, messageID, deliveryID := uuid.NewV7(), uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	var generation int64
	if err := f.Pool.QueryRow(t.Context(), `SELECT run_generation FROM sessions WHERE id=$1`, actorID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	scope := TurnScope{EnvironmentID: f.EnvironmentID, SessionID: actorID, TurnID: turnID, RunID: work.RunID, AttemptNumber: 1, RunGeneration: generation}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_turns (id,environment_id,session_id,sequence,data,status,run_generation,run_id,attempt_number,ready_run_lease_id) VALUES ($1,$2,$3,2,'{}','running',$4,$5,1,$6)`, turnID, f.EnvironmentID, actorID, generation, work.RunID, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET active_turn_id=$2 WHERE id=$1`, actorID, turnID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO session_messages (id,environment_id,session_id,turn_id,run_id,attempt_number,run_generation,data,accepted_sequence,status,delivery_id,delivery_run_lease_id,handling_at) VALUES ($1,$2,$3,$4,$5,1,$6,'{}',1,'handling',$7,$8,clock_timestamp())`, messageID, f.EnvironmentID, actorID, turnID, work.RunID, generation, deliveryID, work.LeaseID)
	setHold := func() {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=$2,dispatch_hold_run_id=current_run_id,dispatch_hold_attempt_number=1,dispatch_hold_run_generation=run_generation,dispatch_hold_reason='interrupt_requested' WHERE id=$1`, actorID, holdID)
	}
	setHold()
	if err := transact(func(tx pgx.Tx) error { return run.EnterExecution(t.Context(), tx, fence, "actor", "test-actor") }); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("first entry under hold: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET dispatch_hold_id=NULL,dispatch_hold_run_id=NULL,dispatch_hold_attempt_number=NULL,dispatch_hold_run_generation=NULL,dispatch_hold_reason=NULL WHERE id=$1`, actorID)
	if err := transact(func(tx pgx.Tx) error { return run.EnterExecution(t.Context(), tx, fence, "actor", "test-actor") }); err != nil {
		t.Fatal(err)
	}
	setHold()
	if err := transact(func(tx pgx.Tx) error { return run.EnterExecution(t.Context(), tx, fence, "actor", "test-actor") }); err != nil {
		t.Fatalf("entry receipt replay under hold: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 millisecond',expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, work.LeaseID)
	var expiry time.Time
	if err := f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	if err := transact(func(tx pgx.Tx) error {
		a, err := run.RenewExecution(t.Context(), tx, fence, expiry)
		if err == nil && !a.Lease().ExpiresAt.Time.After(expiry) {
			t.Fatal("held execution could not renew while draining callbacks")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		err := transact(func(tx pgx.Tx) error {
			if _, err := run.LockLiveExecution(t.Context(), tx, fence); err != nil {
				return err
			}
			if _, err := ValidateTurnWork(t.Context(), tx, scope); !errors.Is(err, ErrTurnStopped) {
				t.Fatalf("new work admitted under hold: %v", err)
			}
			message, err := CompleteMessage(t.Context(), tx, scope, work.LeaseID, messageID, deliveryID, MessageOutcome{Status: "handled"})
			if err == nil && message.Status != "handled" {
				t.Fatalf("message status = %s", message.Status)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var events int
	if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM session_events WHERE message_id=$1 AND kind='message.handled'`, messageID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("completion events=%d: %v", events, err)
	}
	for _, change := range []string{"dispatch_hold_run_generation=run_generation+1", "dispatch_hold_reason='recovery_required'"} {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET `+change+` WHERE id=$1`, actorID)
		err := transact(func(tx pgx.Tx) error { _, err := run.LockLiveExecution(t.Context(), tx, fence); return err })
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("invalid hold accepted: %v", err)
		}
		setHold()
	}
}
