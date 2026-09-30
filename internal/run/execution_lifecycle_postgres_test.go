package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestExecutionStartReplayAndIndependentRenewal(t *testing.T) {
	for _, actor := range []bool{false, true} {
		t.Run(map[bool]string{false: "Task", true: "Actor"}[actor], func(t *testing.T) {
			f, work, fence := executionClaimFixture(t)
			if actor {
				f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
			}
			if _, err := claimExecutionTest(t, f, fence, true); err != nil {
				t.Fatal(err)
			}
			start := func() (Execution, error) {
				tx, e := f.Pool.Begin(t.Context())
				if e != nil {
					return Execution{}, e
				}
				defer tx.Rollback(context.Background())
				a, e := StartExecution(t.Context(), tx, fence)
				if e == nil {
					e = tx.Commit(t.Context())
				}
				return a, e
			}
			a, err := start()
			if err != nil {
				t.Fatal(err)
			}
			replay, err := start()
			if err != nil {
				t.Fatal(err)
			}
			if a.Run().ActiveStartedAt != replay.Run().ActiveStartedAt || a.Lease().StartedAt != replay.Lease().StartedAt || a.Run().Revision != replay.Run().Revision {
				t.Fatal("start replay reset active budget")
			}
			if actor {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE sessions SET committed_input_sequence=2 WHERE current_run_id=$1`, work.RunID)
			}
			// Keep the actual start receipt, but shorten the test grant to make renewal due.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 millisecond',expires_at=clock_timestamp()+interval '2 seconds' WHERE id=$1`, work.LeaseID)
			var expected time.Time
			if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&expected); err != nil {
				t.Fatal(err)
			}
			renew := func(expiry time.Time) (Execution, error) {
				tx, e := f.Pool.Begin(t.Context())
				if e != nil {
					return Execution{}, e
				}
				defer tx.Rollback(context.Background())
				a, e := RenewExecution(t.Context(), tx, fence, expiry)
				if e == nil {
					e = tx.Commit(t.Context())
				}
				return a, e
			}
			renewed, err := renew(expected)
			if err != nil {
				t.Fatal(err)
			}
			if !renewed.Lease().ExpiresAt.Time.After(expected) {
				t.Fatal("grant not extended")
			}
			replay, err = renew(expected)
			if err != nil || replay.Lease().ExpiresAt != renewed.Lease().ExpiresAt {
				t.Fatalf("renew replay changed receipt: %v", err)
			}
			if _, err = renew(expected.Add(-time.Second)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("unknown renewal receipt=%v", err)
			}
			var unchanged bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT writer_expires_at=$2 AND writer_generation=$3 FROM computer_instances WHERE id=$1`, a.Instance().ID, a.Instance().WriterExpiresAt, a.Instance().WriterGeneration).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("Run renewal changed physical writer: %v %v", unchanged, err)
			}
		})
	}
}

func TestExecutionStartRejectsUnclaimedGrant(t *testing.T) {
	f, _, fence := executionClaimFixture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = StartExecution(t.Context(), tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unclaimed start=%v", err)
	}
}

func TestExecutionRenewalHonorsActiveBudget(t *testing.T) {
	f, work, fence := executionClaimFixture(t)
	if _, err := claimExecutionTest(t, f, fence, true); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET max_active_duration_ms=10000 WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 millisecond',expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, work.LeaseID)
	var expiry time.Time
	if err = f.Pool.QueryRow(t.Context(), `SELECT expires_at FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&expiry); err != nil {
		t.Fatal(err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	a, err := RenewExecution(t.Context(), tx, fence, expiry)
	if err != nil {
		t.Fatal(err)
	}
	deadline := a.Run().ActiveStartedAt.Time.Add(10 * time.Second)
	if !a.Lease().ExpiresAt.Time.Equal(deadline) {
		t.Fatalf("expiry %s != budget %s", a.Lease().ExpiresAt.Time, deadline)
	}
}

func TestExecutionEntrypointIsFencedAndIdempotent(t *testing.T) {
	f, work, fence := executionClaimFixture(t)
	enter := func(kind, id string, commit bool) error {
		tx, e := f.Pool.Begin(t.Context())
		if e != nil {
			return e
		}
		defer tx.Rollback(context.Background())
		e = EnterExecution(t.Context(), tx, fence, kind, id)
		if e == nil && commit {
			e = tx.Commit(t.Context())
		}
		return e
	}
	if err := enter("task", "test-task", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unstarted entrypoint=%v", err)
	}
	if _, err := claimExecutionTest(t, f, fence, true); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = StartExecution(t.Context(), tx, fence); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = enter("actor", "test-task", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong entrypoint kind=%v", err)
	}
	if err = enter("task", "another-task", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("wrong entrypoint ID=%v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	if err = enter("task", "test-task", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("draining Instance allowed first entry: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='open' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	if err = enter("task", "test-task", false); err != nil {
		t.Fatal(err)
	}
	var absent bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at IS NULL FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&absent); err != nil || !absent {
		t.Fatalf("entrypoint rollback=%v %v", absent, err)
	}
	if err = enter("task", "test-task", true); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='draining' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	var first, second time.Time
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err = enter("task", "test-task", true); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT entrypoint_entered_at FROM run_attempts WHERE run_id=$1 AND number=1`, work.RunID).Scan(&second); err != nil || !first.Equal(second) {
		t.Fatalf("entrypoint replay changed timestamp: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	if err = enter("task", "test-task", true); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired Instance accepted entrypoint replay: %v", err)
	}
}

func TestLiveExecutionRetainsWaitAuthorityWithoutChangingOwnership(t *testing.T) {
	f, work, fence := executionClaimFixture(t)
	if _, err := claimExecutionTest(t, f, fence, true); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	started, err := StartExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET status='waiting' WHERE id=$1`, work.RunID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='checkpointing' WHERE id=$1`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='checkpointing' WHERE id=$1`, started.Instance().ID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	live, err := LockLiveExecution(t.Context(), tx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if live.Instance().WriterGeneration != started.Instance().WriterGeneration || live.Instance().WriterExpiresAt != started.Instance().WriterExpiresAt || live.Lease().ExpiresAt != started.Lease().ExpiresAt {
		t.Fatal("observation changed physical or logical lease")
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET status='finalizing',finalization_operation_id=gen_random_uuid(),finalization_started_at=clock_timestamp(),finalization_request_fingerprint='sha256:'||repeat('0',64) WHERE id=$1`, work.LeaseID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = LockLiveExecution(t.Context(), tx, fence); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("finalizing grant accepted live mutation: %v", err)
	}
}
