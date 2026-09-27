package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestRunPlacementRejectsQueueDeadlineAfterAuthorityLockWait(t *testing.T) {
	for _, placement := range []string{"new Instance", "shared Instance"} {
		for _, lock := range []string{"Worker", "Computer"} {
			t.Run(placement+"/"+lock, func(t *testing.T) {
				f, work, a := commandPlacementFixture(t)
				candidate := queuedSharedRun(t, f, work)
				if placement == "new Instance" {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
				}
				snapshot := func() string {
					t.Helper()
					var value string
					if err := f.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object('computer',to_jsonb(c),'instances',(SELECT jsonb_agg(to_jsonb(i) ORDER BY id) FROM computer_instances i WHERE computer_id=c.id),'leases',(SELECT jsonb_agg(to_jsonb(l) ORDER BY id) FROM run_leases l WHERE run_id=$1))::text FROM computers c WHERE id=(SELECT computer_id FROM runs WHERE id=$1)`, candidate.RunID).Scan(&value); err != nil {
						t.Fatal(err)
					}
					return value
				}
				before := snapshot()
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				blocker, err := f.Pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer blocker.Rollback(context.Background())
				var blockerPID int32
				if err = blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				if lock == "Worker" {
					dbtest.MustExec(t, ctx, blocker, `SELECT id FROM worker_hosts WHERE id=$1 FOR UPDATE`, f.WorkerID)
				} else {
					dbtest.MustExec(t, ctx, blocker, `SELECT id FROM computers WHERE id=(SELECT computer_id FROM runs WHERE id=$1) FOR UPDATE`, candidate.RunID)
				}
				dbtest.MustExec(t, ctx, f.Pool, `UPDATE runs SET queued_expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, candidate.RunID)
				done := make(chan error, 1)
				go func() { _, e := a.PlaceReadyRun(ctx, candidate); done <- e }()
				for {
					var blocked, expired bool
					if err = f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity a WHERE $1=ANY(pg_blocking_pids(a.pid))), queued_expires_at<clock_timestamp() FROM runs WHERE id=$2`, blockerPID, candidate.RunID).Scan(&blocked, &expired); err != nil {
						t.Fatal(err)
					}
					if blocked && expired {
						break
					}
					select {
					case e := <-done:
						t.Fatalf("placement did not wait: %v", e)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					case <-time.After(10 * time.Millisecond):
					}
				}
				if err = blocker.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case err = <-done:
					if !errors.Is(err, ErrCandidateChanged) {
						t.Fatalf("expired placement=%v", err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if after := snapshot(); after != before {
					t.Fatalf("rejected placement mutated physical authority or granted execution\nbefore=%s\nafter=%s", before, after)
				}
			})
		}
	}
}

func TestRunPlacementKeepsAlreadyStartedRunEligible(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	candidate := queuedSharedRun(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET first_lease_at=clock_timestamp(),queued_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, candidate.RunID)
	placed, err := a.PlaceReadyRun(t.Context(), candidate)
	if err != nil || !placed.LeaseCreated {
		t.Fatalf("already started Run blocked by queue deadline: %+v %v", placed, err)
	}
}
