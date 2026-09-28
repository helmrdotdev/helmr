package dispatch

import (
	"context"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerRecoveryPreparationBudget(t *testing.T) {
	for _, member := range []string{"Run", "Command"} {
		t.Run(member, func(t *testing.T) {
			f, work, a := commandPlacementFixture(t)
			runCandidate := queuedSharedRun(t, f, work)
			commandCandidate := pendingSharedCommand(t, f, work)
			var computerID, root pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT c.id,c.head_disk_version_id FROM computers c JOIN run_leases l ON l.computer_id=c.id WHERE l.id=$1`, work.LeaseID).Scan(&computerID, &root); err != nil {
				t.Fatal(err)
			}
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=2,observed_state='closed',observed_desired_version=2,terminal_at=now(),terminal_reason_code='test_exclusion',reclaimed_at=now(),reclaim_evidence='{"method":"host_reconciled"}',admission_state='closed',mount_state='unmounted',unmounted_at=now() WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET epoch_guest_ephemeral_disk_bytes=68719476736,per_vm_guest_ephemeral_disk_bytes=34359738368 WHERE id=$1`, f.WorkerID)
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_id=$2,recovery_disk_version_id=head_disk_version_id,recovery_reason='worker_lost',recovery_started_at=now() WHERE id=$1`, computerID, uuid.NewV7())
			place := func() (pgtype.UUID, error) {
				if member == "Run" {
					p, e := a.PlaceReadyRun(t.Context(), runCandidate)
					return p.ComputerInstanceID, e
				}
				p, e := a.PlaceComputerCommand(t.Context(), commandCandidate)
				return p.ComputerInstanceID, e
			}
			for count := 1; count <= 8; count++ {
				id, err := place()
				if err != nil {
					t.Fatalf("attempt %d: %v", count, err)
				}
				replay, err := place()
				if err != nil || replay != id {
					t.Fatalf("allocation replay=%v %v", replay, err)
				}
				var attempts int
				if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count FROM computers WHERE id=$1`, computerID).Scan(&attempts); err != nil || attempts != count {
					t.Fatalf("attempts=%d want=%d: %v", attempts, count, err)
				}
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=now()-interval '1 second' WHERE id=$1`, id)
				if n, e := a.ReconcileComputerInstances(t.Context(), 10); e != nil || n != 1 {
					t.Fatalf("expiry=%d %v", n, e)
				}
				if count < 8 {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET next_preparation_at=now()-interval '1 second' WHERE id=$1`, computerID)
				}
				// Closing authority must not release capacity or allow a replacement until physical exclusion is recorded.
				if next, e := place(); e == nil && next != id {
					t.Fatalf("replacement admitted before reclaim: %v", next)
				}
				i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
				if err != nil {
					t.Fatal(err)
				}
				if i.ReclaimedAt.Valid || i.DesiredState != "closed" {
					t.Fatalf("expiry released physical ownership: %+v", i)
				}
				tx, err := f.Pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, err = RecordComputerInstanceReclaim(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), db.ReclaimComputerInstanceParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, Reason: pgvalue.Text("preparation_expired"), Evidence: []byte(`{"method":"host_reconciled"}`)})
				if err != nil {
					tx.Rollback(context.Background())
					t.Fatal(err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if count < 8 {
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET next_preparation_at=now()+interval '1 hour' WHERE id=$1`, computerID)
					if _, err = place(); err == nil {
						t.Fatal("preparation backoff bypassed")
					}
					dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET next_preparation_at=now()-interval '1 second' WHERE id=$1`, computerID)
				}
			}
			var exhausted bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT preparation_failure->>'code'='computer_preparation_exhausted' AND desired_state='stopped' AND next_preparation_at IS NULL FROM computers WHERE id=$1`, computerID).Scan(&exhausted); err != nil || !exhausted {
				t.Fatalf("Computer exhaustion not recorded: %v %v", exhausted, err)
			}
			// Fresh members isolate the Computer budget from terminal member guards.
			runCandidate = queuedSharedRun(t, f, work)
			commandCandidate = pendingSharedCommand(t, f, work)
			if _, err := place(); err == nil {
				t.Fatal("ninth preparation admitted")
			}
			var attempts, live int
			var source pgtype.UUID
			if err := f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count,recovery_disk_version_id,(SELECT count(*) FROM computer_instances WHERE computer_id=c.id AND reclaimed_at IS NULL) FROM computers c WHERE id=$1`, computerID).Scan(&attempts, &source, &live); err != nil || attempts != 8 || source != root || live != 0 {
				t.Fatalf("attempts=%d source=%v live=%d: %v", attempts, source, live, err)
			}
		})
	}
}
