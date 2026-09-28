package db_test

import (
	"errors"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestIdleReadyInstanceDoesNotExpireWithPreparationDeadline(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 hour' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	candidates, err := db.New(f.Pool).ListExpiredComputerInstances(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ready Instance expired: %+v", candidates)
	}
}
func TestInstanceExpiryRetainsPhysicalReservation(t *testing.T) {
	for _, preparing := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "preparation"}[preparing], func(t *testing.T) {
			f := runtest.New(t)
			work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
			if preparing {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL,preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			} else {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
			}
			q := db.New(f.Pool)
			candidates, err := q.ListExpiredComputerInstances(t.Context(), 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(candidates) != 1 {
				t.Fatalf("candidates=%d", len(candidates))
			}
			i := candidates[0]
			tx, err := f.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			locked := db.New(tx)
			if _, err = locked.LockComputer(t.Context(), db.LockComputerParams{EnvironmentID: i.EnvironmentID, ID: i.ComputerID}); err != nil {
				t.Fatal(err)
			}
			if _, err = locked.LockComputerInstance(t.Context(), db.LockComputerInstanceParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID}); err != nil {
				t.Fatal(err)
			}
			params := db.ExpireComputerInstanceParams{ID: i.ID, EnvironmentID: i.EnvironmentID, WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion}
			closed, err := locked.ExpireComputerInstance(t.Context(), params)
			if err != nil {
				t.Fatal(err)
			}
			if closed.DesiredState != "closed" || closed.AdmissionState != "closed" || closed.ReclaimedAt.Valid || len(closed.ReclaimEvidence) > 0 || closed.ReservedCPUMillis != i.ReservedCPUMillis || closed.WriterGeneration != i.WriterGeneration || closed.SourceDiskVersionID != i.SourceDiskVersionID {
				t.Fatalf("expiry released physical authority: %+v", closed)
			}
			if _, err = locked.ExpireComputerInstance(t.Context(), params); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("expiry replay=%v", err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			var live bool
			if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_lease_id=$2 FROM runs WHERE id=$1`, work.RunID, work.LeaseID).Scan(&live); err != nil {
				t.Fatal(err)
			}
			if !live {
				t.Fatal("physical expiry directly settled logical Run")
			}
			candidates, err = q.ListExpiredComputerInstances(t.Context(), 10)
			if err != nil || len(candidates) != 0 {
				t.Fatalf("closed rediscovered=%+v err=%v", candidates, err)
			}
		})
	}
}
func TestInstanceExpiryRechecksRenewedWriter(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()-interval '1 second' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	q := db.New(f.Pool)
	candidates, err := q.ListExpiredComputerInstances(t.Context(), 1)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("discovery=%+v %v", candidates, err)
	}
	i := candidates[0]
	// Change the candidate before the expiry decision; the query must revalidate.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET writer_expires_at=clock_timestamp()+interval '1 minute' WHERE id=$1`, i.ID)
	if _, err = q.ExpireComputerInstance(t.Context(), db.ExpireComputerInstanceParams{ID: i.ID, EnvironmentID: pgvalue.UUID(f.EnvironmentID), WriterGeneration: i.WriterGeneration, DesiredVersion: i.DesiredVersion}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale expiry applied: %v", err)
	}
}
