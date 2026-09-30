package run

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestLiveExecutionLocksAddressedSessionAndComputer(t *testing.T) {
	f, source, fence := executionClaimFixture(t)
	target := f.AddRunLease(t, "running", time.Now())
	targetID := f.ConvertToActor(t, t.Context(), target, `{"enabled":false}`)
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
	var targetComputer pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_id FROM sessions WHERE id=$1`, targetID).Scan(&targetComputer); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"session", "computer"} {
		t.Run(kind, func(t *testing.T) {
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(context.Background())
			var a Execution
			if kind == "session" {
				a, err = LockLiveExecutionForSession(t.Context(), tx, fence, pgvalue.UUID(targetID))
			} else {
				a, err = LockLiveExecutionForComputer(t.Context(), tx, fence, targetComputer)
			}
			if err != nil || a.Run().ID != pgvalue.UUID(source.RunID) {
				t.Fatalf("source=%s error=%v", pgvalue.UUIDString(a.Run().ID), err)
			}
			probe, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Rollback(context.Background())
			_, err = probe.Exec(t.Context(), `SELECT id FROM computers WHERE id=$1 FOR UPDATE NOWAIT`, targetComputer)
			if err == nil {
				t.Fatal("target Computer was not locked with source")
			}
			if err = probe.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if kind == "session" {
				probe, err = f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer probe.Rollback(context.Background())
				_, err = probe.Exec(t.Context(), `SELECT id FROM sessions WHERE id=$1 FOR UPDATE NOWAIT`, targetID)
				if err == nil {
					t.Fatal("target Session was not locked with source")
				}
			}
		})
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = LockLiveExecutionForSession(t.Context(), tx, fence, pgvalue.UUID(uuid.NewV7())); !errors.Is(err, ErrExecutionTargetNotFound) {
		t.Fatalf("missing target=%v", err)
	}
}

func TestLiveExecutionTargetRechecksDeadlineAfterSessionLock(t *testing.T) {
	f, _, fence := executionClaimFixture(t)
	target := f.AddRunLease(t, "running", time.Now())
	targetID := f.ConvertToActor(t, t.Context(), target, `{"enabled":false}`)
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
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE run_leases SET start_deadline_at=clock_timestamp()-interval '1 second',expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, fence.LeaseID)
	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), blocker, `SELECT id FROM sessions WHERE id=$1 FOR UPDATE`, targetID)
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := LockLiveExecutionForSession(t.Context(), tx, fence, pgvalue.UUID(targetID))
		result <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock'), (SELECT expires_at<=clock_timestamp() FROM run_leases WHERE id=$2)`, pid, fence.LeaseID).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source did not wait for target Session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expired source accepted after target lock: %v", err)
	}
}
