package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
)

func TestAuthorityTransactionsReadCommitted(t *testing.T) {
	_, _, authority := commandPlacementFixture(t)
	tx, err := authority.begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), tx)
	var isolation string
	if err := tx.QueryRow(t.Context(), `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		t.Fatal(err)
	}
	if isolation != "read committed" {
		t.Fatalf("placement transaction isolation = %q, want read committed", isolation)
	}
}

func TestConcurrentRunPlacementRechecksLockedWorkerCapacity(t *testing.T) {
	f, first, authority := commandPlacementFixture(t)
	second := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	candidates := []ReadyRunCandidate{queuedSharedRun(t, f, first), queuedSharedRun(t, f, second)}
	for _, candidate := range candidates {
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET concurrency_key=id::text WHERE id=$1`, candidate.RunID)
	}
	// Supply consists of two persistent Computers with no remaining physical instances.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now()`)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET max_vm_slots=1,max_vm_starts=1,epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)

	blocker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(context.Background(), blocker)
	if _, err := blocker.Exec(t.Context(), `
SELECT id FROM worker_groups WHERE id = $1 FOR UPDATE`, pgvalue.UUID(runtest.WorkerGroupID)); err != nil {
		t.Fatal(err)
	}

	results := make(chan error, len(candidates))
	for _, row := range candidates {
		candidate := ReadyRunCandidate{
			OrgID: row.OrgID, RunID: row.RunID,
			ExpectedRunRevision: row.ExpectedRunRevision,
		}
		go func() {
			_, err := authority.PlaceReadyRun(t.Context(), candidate)
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid))`, blocker.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == len(candidates) {
			break
		}
		select {
		case err := <-results:
			t.Fatalf("placement returned before Worker fence: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("blocked placements=%d", waiting)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}

	succeeded := 0
	capacityRejected := 0
	for range candidates {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrCapacityUnavailable):
			capacityRejected++
		default:
			t.Fatalf("concurrent placement failed: %v", err)
		}
	}
	if succeeded != 1 || capacityRejected != 1 {
		t.Fatalf("placements succeeded=%d capacity_rejected=%d, want 1 and 1", succeeded, capacityRejected)
	}

	var reservations int
	if err := f.Pool.QueryRow(t.Context(), `
SELECT count(*)
  FROM computer_instances
 WHERE worker_host_id = $1
   AND worker_epoch = 1
   AND reclaimed_at IS NULL`, f.WorkerID).Scan(&reservations); err != nil {
		t.Fatal(err)
	}
	if reservations != 1 {
		t.Fatalf("live runtime reservations = %d, want 1", reservations)
	}
}
