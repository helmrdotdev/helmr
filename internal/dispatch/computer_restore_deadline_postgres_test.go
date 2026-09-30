package dispatch_test

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/computer"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestComputerRestoreRejectsCheckpointExpiryAfterLockWait(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(map[bool]string{false: "Tasks", true: "Actor and Task"}[actor], func(t *testing.T) {
			f, a, fence := dispatchtest.Restore(t, false, func(f runtest.Fixture, work runtest.RunLease) {
				if actor {
					f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
				}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			dbtest.MustExec(t, ctx, f.Pool, `UPDATE computer_checkpoints SET expires_at=clock_timestamp()+interval '2 seconds' WHERE id=(SELECT source_checkpoint_id FROM computer_instances WHERE id=$1)`, fence.ID)
			blocker, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Rollback(context.Background())
			dbtest.MustExec(t, ctx, blocker, `SELECT id FROM computer_checkpoints WHERE id=(SELECT source_checkpoint_id FROM computer_instances WHERE id=$1) FOR UPDATE`, fence.ID)
			tx, err := f.Pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var pid int32
			if err = tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, e := a.CommitComputerRestore(ctx, tx, fence); done <- e }()
			for {
				var blocked, expired bool
				if err = f.Pool.QueryRow(ctx, `SELECT coalesce((SELECT wait_event_type='Lock' FROM pg_stat_activity WHERE pid=$1),false),cp.expires_at<clock_timestamp() FROM computer_checkpoints cp JOIN computer_instances i ON i.source_checkpoint_id=cp.id WHERE i.id=$2`, pid, fence.ID).Scan(&blocked, &expired); err != nil {
					t.Fatal(err)
				}
				if blocked && expired {
					break
				}
				select {
				case e := <-done:
					t.Fatalf("restore did not wait: %v", e)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err = blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case e := <-done:
				if !errors.Is(e, pgx.ErrNoRows) {
					t.Fatalf("expired restore accepted: %v", e)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err = tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var untouched bool
			if err = f.Pool.QueryRow(ctx, `SELECT i.admission_state='restoring' AND c.resume_committed_at IS NULL AND c.resume_computer_instance_id IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2) AND NOT EXISTS(SELECT 1 FROM run_waits WHERE suspend_checkpoint_id=c.id AND (current_run_lease_id IS NOT NULL OR suspension_status<>'parked')) FROM computer_instances i JOIN computer_checkpoints c ON c.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("expired activation changed authority: %v %v", untouched, err)
			}
		})
	}
}

func TestRestorePreparationExpiryPreservesCapturedWaits(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(map[bool]string{false: "Tasks", true: "Actor and Task"}[actor], func(t *testing.T) {
			f, a, fence := dispatchtest.Restore(t, false, func(f runtest.Fixture, work runtest.RunLease) {
				if actor {
					f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
				}
			})
			snapshot := func() string {
				t.Helper()
				var value string
				if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_agg(to_jsonb(w) ORDER BY w.id)::text FROM run_waits w JOIN computer_instances i ON i.source_checkpoint_id=w.suspend_checkpoint_id WHERE i.id=$1`, fence.ID).Scan(&value); err != nil {
					t.Fatal(err)
				}
				if value == "" || value == "null" {
					t.Fatal("fixture has no captured waits")
				}
				return value
			}
			before := snapshot()
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, fence.ID)
			for range 2 {
				if _, err := a.ReconcileComputerInstances(t.Context(), 10); err != nil {
					t.Fatal(err)
				}
			}
			if after := snapshot(); after != before {
				t.Fatal("pre-commit preparation expiry changed captured waits")
			}
			var untouched bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT i.desired_state='closed' AND i.reclaimed_at IS NULL AND cp.status='ready' AND cp.resume_committed_at IS NULL AND cp.resume_computer_instance_id IS NULL AND NOT EXISTS(SELECT 1 FROM run_leases WHERE computer_instance_id=i.id) AND NOT EXISTS(SELECT 1 FROM control_outbox WHERE topic=$2) FROM computer_instances i JOIN computer_checkpoints cp ON cp.id=i.source_checkpoint_id WHERE i.id=$1`, fence.ID, computer.RestoreActivationTopic).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("expiry lost captured authority or granted new execution: %v %v", untouched, err)
			}
		})
	}
}
