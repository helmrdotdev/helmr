package computer_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/dispatch/dispatchtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

// restorePlanFixture is a restoring Instance with a derivable write
// capability, optionally with its restore committed.
type restorePlanFixture struct {
	runtest.Fixture
	authority *dispatch.Authority
	ref       computer.InstanceRef
	principal workergroup.HostPrincipal
	writer    computer.WriterRef
	key       disk.FencingKey
}

func newRestorePlanFixture(t *testing.T, idle, committed bool) restorePlanFixture {
	t.Helper()
	f, authority, ref := dispatchtest.Restore(t, idle)
	key, err := disk.NewFencingKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{ID: pgvalue.UUID(ref.ID), EnvironmentID: pgvalue.UUID(f.EnvironmentID)})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := computer.WriterTokenHash(key, ref.ID, pgvalue.MustUUIDValue(i.ComputerID), i.WriterGeneration)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_token_hash=$2 WHERE id=$1`, ref.ID, hash)
	rf := restorePlanFixture{
		Fixture: f, authority: authority, ref: ref, key: key,
		principal: hostPrincipal(t, f),
		writer:    computer.WriterRef{EnvironmentID: f.EnvironmentID, InstanceID: ref.ID, WriterGeneration: i.WriterGeneration},
	}
	if committed {
		rf.commit(t)
	}
	return rf
}

func hostPrincipal(t *testing.T, f runtest.Fixture) workergroup.HostPrincipal {
	t.Helper()
	principal := workergroup.HostPrincipal{HostID: f.WorkerID, GroupID: runtest.WorkerGroupID, Epoch: 1}
	if err := f.Pool.QueryRow(t.Context(), `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1`, f.WorkerID).Scan(&principal.HostClaimVersion, &principal.GroupClaimVersion); err != nil {
		t.Fatal(err)
	}
	return principal
}

func (f restorePlanFixture) commit(t *testing.T) db.ComputerCheckpoint {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	cp, err := f.authority.CommitComputerRestore(t.Context(), tx, f.ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	return cp
}

func (f restorePlanFixture) read(ctx context.Context) (*computer.RestorePlan, error) {
	return computer.ReadRestorePlan(ctx, f.Pool, f.key, f.principal, f.writer)
}

func TestRestorePlanCommittedWholeSet(t *testing.T) {
	for _, idle := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "empty"}[idle], func(t *testing.T) {
			f := newRestorePlanFixture(t, idle, true)
			for range 2 {
				plan, err := f.read(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				want := 2
				if idle {
					want = 0
				}
				if plan == nil || len(plan.Members) != want || plan.Instance.ID != pgvalue.UUID(f.ref.ID) || plan.Instance.WriterGeneration != f.writer.WriterGeneration || plan.WriteCapability == "" {
					t.Fatalf("incomplete restore plan (members expected %d)", want)
				}
				for _, member := range plan.Members {
					if member.AttemptNumber != 1 || member.LeaseID == "" || member.LeaseSequence != 2 || member.ExpiresAt.IsZero() {
						t.Fatal("incomplete member authority")
					}
				}
			}
		})
	}
}

func TestRestorePlanWaitsForCommit(t *testing.T) {
	f := newRestorePlanFixture(t, false, false)
	plan, err := f.read(t.Context())
	if err != nil || plan != nil {
		t.Fatalf("uncommitted plan present=%v err=%v", plan != nil, err)
	}
}

func TestRestorePlanRejectsIncompleteOrStaleAuthority(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"expired member", `UPDATE run_leases SET created_at=clock_timestamp()-interval '3 seconds',start_deadline_at=clock_timestamp()-interval '2 seconds',expires_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`},
		{"missed start deadline", `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 second' WHERE computer_instance_id=$1`},
		{"advanced writer", `UPDATE computers SET writer_generation=writer_generation+1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`},
		{"changed wait", `UPDATE run_waits SET suspension_status='hot',prior_run_lease_id=NULL WHERE current_run_lease_id IN (SELECT id FROM run_leases WHERE computer_instance_id=$1)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestorePlanFixture(t, false, true)
			dbtest.MustExec(t, t.Context(), f.Pool, tc.sql, f.ref.ID)
			if _, err := f.read(t.Context()); !errors.Is(err, computer.ErrAuthorityChanged) {
				t.Fatalf("stale plan err=%v", err)
			}
		})
	}
}

func TestRestorePlanRechecksDeadlineAfterMemberLock(t *testing.T) {
	f := newRestorePlanFixture(t, false, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := f.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	var pid int32
	if err := blocker.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	// Leave the row unchanged while blocking the reader. PostgreSQL can evaluate
	// its time predicate before waiting for this lock.
	dbtest.MustExec(t, ctx, f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()+interval '1 second' WHERE computer_instance_id=$1`, f.ref.ID)
	if _, err := blocker.Exec(ctx, `SELECT id FROM run_leases WHERE computer_instance_id=$1 ORDER BY run_id FOR UPDATE`, f.ref.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var planErr error
	go func() { defer close(done); _, planErr = f.read(ctx) }()
	defer func() { cancel(); <-done }()
	for {
		var blocked bool
		if err := f.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-done:
			t.Fatalf("plan finished before lease lock: %v", planErr)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	for {
		var expired bool
		if err := f.Pool.QueryRow(ctx, `SELECT bool_and(start_deadline_at<=clock_timestamp()) FROM run_leases WHERE computer_instance_id=$1`, f.ref.ID).Scan(&expired); err != nil {
			t.Fatal(err)
		}
		if expired {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if !errors.Is(planErr, computer.ErrAuthorityChanged) {
			t.Fatalf("expired grant plan: %v", planErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestRestorePlanAfterParkedWakeup(t *testing.T) {
	for _, wakeCount := range []int{1, 2} {
		t.Run(fmt.Sprintf("woken-%d", wakeCount), func(t *testing.T) {
			f := newRestorePlanFixture(t, false, false)
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			rows, err := tx.Query(t.Context(), `SELECT w.run_id,w.expected_run_revision,w.attempt_number,w.id,w.prior_run_lease_id,w.suspend_checkpoint_id
 FROM run_waits w JOIN computer_checkpoint_runs m ON m.run_wait_id=w.id
 JOIN computer_instances i ON i.source_checkpoint_id=m.checkpoint_id WHERE i.id=$1 ORDER BY w.run_id LIMIT $2`, f.ref.ID, wakeCount)
			if err != nil {
				t.Fatal(err)
			}
			var wakeups []db.ResolveParkedTokenWaitParams
			for rows.Next() {
				var wake db.ResolveParkedTokenWaitParams
				if err := rows.Scan(&wake.RunID, &wake.ExpectedRunRevision, &wake.AttemptNumber, &wake.WaitID, &wake.PriorRunLeaseID, &wake.SuspendCheckpointID); err != nil {
					t.Fatal(err)
				}
				wake.ConditionStatus = "completed"
				wake.ConditionResult = []byte(`{"resume":true}`)
				wake.ConditionError = nil
				wakeups = append(wakeups, wake)
			}
			err = rows.Err()
			rows.Close()
			if err != nil || len(wakeups) != wakeCount {
				t.Fatalf("wakeups=%d err=%v", len(wakeups), err)
			}
			for _, wake := range wakeups {
				if _, err := db.New(tx).ResolveParkedTokenWait(t.Context(), wake); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var queued int
			if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM runs r JOIN run_waits w ON w.run_id=r.id WHERE r.status='queued' AND w.suspension_status='resume_pending' AND w.expected_run_revision=r.revision`).Scan(&queued); err != nil || queued != wakeCount {
				t.Fatalf("queued=%d err=%v", queued, err)
			}
			cp := f.commit(t)
			plan, err := f.read(t.Context())
			if err != nil || plan == nil || len(plan.Members) != 2 {
				t.Fatalf("whole restored set missing: plan=%+v err=%v", plan, err)
			}
			var grants []dispatch.ComputerRestoreGrant
			for _, member := range plan.Members {
				grants = append(grants, dispatch.ComputerRestoreGrant{RunID: pgvalue.UUID(uuid.MustParse(member.RunID)), LeaseID: pgvalue.UUID(uuid.MustParse(member.LeaseID)), LeaseSequence: member.LeaseSequence})
			}
			tx, err = f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			if _, err := dispatch.AcknowledgeComputerRestore(t.Context(), tx, f.ref, cp.ID, f.writer.WriterGeneration, grants); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var resumed, completed, pending int
			err = f.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE w.condition_status='completed' AND w.condition_result='{"resume":true}'::jsonb),count(*) FILTER (WHERE w.condition_status='pending')
 FROM run_waits w JOIN runs r ON r.id=w.run_id JOIN run_leases l ON l.id=w.current_run_lease_id
 WHERE l.computer_instance_id=$1 AND l.status='running' AND r.status='waiting' AND w.suspension_status='resuming' AND w.expected_run_revision=r.revision`, f.ref.ID).Scan(&resumed, &completed, &pending)
			if err != nil || resumed != 2 || completed != wakeCount || pending != 2-wakeCount {
				t.Fatalf("resumed=%d completed=%d pending=%d err=%v", resumed, completed, pending, err)
			}
		})
	}
}
