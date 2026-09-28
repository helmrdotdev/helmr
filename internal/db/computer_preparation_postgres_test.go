package db_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

func preparingComputer(t *testing.T, count int) (runtest.Fixture, db.ComputerInstance) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	var i db.ComputerInstance
	var err error
	i, err = db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: instanceIDForLease(t, f, work)})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=$2,preparation_instance_id=$3 WHERE id=$1`, i.ComputerID, count, i.ID)
	return f, i
}
func instanceIDForLease(t *testing.T, f runtest.Fixture, work runtest.RunLease) (id pgtype.UUID) {
	t.Helper()
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
func preparationFailureParams(i db.ComputerInstance) db.SettleComputerPreparationFailureParams {
	return db.SettleComputerPreparationFailureParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID, InstanceID: i.ID}
}
func completePreparationParams(i db.ComputerInstance) db.CompleteComputerPreparationParams {
	return db.CompleteComputerPreparationParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID, InstanceID: i.ID, DesiredVersion: i.DesiredVersion}
}
func closePreparation(t *testing.T, f runtest.Fixture, i db.ComputerInstance) {
	t.Helper()
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed' WHERE id=$1`, i.ID)
}
func TestPreparationEighthAttemptCanSucceed(t *testing.T) {
	f, i := preparingComputer(t, 8)
	c, err := db.New(f.Pool).CompleteComputerPreparation(t.Context(), completePreparationParams(i))
	if err != nil {
		t.Fatal(err)
	}
	if c.PreparationAttemptCount != 0 || c.PreparationInstanceID.Valid || c.NextPreparationAt.Valid || len(c.PreparationFailure) > 0 {
		t.Fatalf("budget not reset: %+v", c)
	}
	// A ready observation replay cannot reset a later preparation.
	if _, err = db.New(f.Pool).CompleteComputerPreparation(t.Context(), completePreparationParams(i)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("completion replay=%v", err)
	}
}
func TestPreparationExhaustionPreservesDataAndDeletion(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "deleting"}[deleting], func(t *testing.T) {
			f, i := preparingComputer(t, 8)
			if deleting {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET status='deleting',desired_state='deleted' WHERE id=$1`, i.ComputerID)
			}
			closePreparation(t, f, i)
			q := db.New(f.Pool)
			c, err := q.SettleComputerPreparationFailure(t.Context(), preparationFailureParams(i))
			if err != nil {
				t.Fatal(err)
			}
			want := "stopped"
			if deleting {
				want = "deleted"
			}
			if c.DesiredState != want || c.PreparationAttemptCount != 8 || len(c.PreparationFailure) == 0 || c.RecoveryID.Valid || c.DirtyState == "dirty_state_lost" || !c.HeadDiskVersionID.Valid {
				t.Fatalf("invalid exhaustion: %+v", c)
			}
			if _, err = q.TouchComputerForAdmission(t.Context(), db.TouchComputerForAdmissionParams{EnvironmentID: c.EnvironmentID, ID: c.ID, ExpectedRevision: c.Revision}); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("exhausted admission=%v", err)
			}
			if _, err = q.CompleteComputerPreparation(t.Context(), completePreparationParams(i)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("late success=%v", err)
			}
			if _, err = q.SettleComputerPreparationFailure(t.Context(), preparationFailureParams(i)); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("failure replay=%v", err)
			}
		})
	}
}
func TestPreparationFailureDiscoveryAndConcurrentReplay(t *testing.T) {
	f, i := preparingComputer(t, 3)
	q := db.New(f.Pool)
	rows, err := q.ListFailedComputerPreparations(t.Context(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("live discovery=%v %v", rows, err)
	}
	closePreparation(t, f, i)
	// The physical fact remains discoverable without any producer-side budget write.
	rows, err = q.ListFailedComputerPreparations(t.Context(), 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("failure discovery=%v %v", rows, err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			tx, e := f.Pool.Begin(t.Context())
			if e != nil {
				results <- e
				return
			}
			defer tx.Rollback(t.Context())
			queries := db.New(tx)
			if _, e = queries.LockComputer(t.Context(), db.LockComputerParams{EnvironmentID: i.EnvironmentID, ID: i.ComputerID}); e == nil {
				_, e = queries.LockComputerInstance(t.Context(), db.LockComputerInstanceParams{EnvironmentID: i.EnvironmentID, ComputerID: i.ComputerID})
			}
			if e == nil {
				_, e = queries.SettleComputerPreparationFailure(t.Context(), preparationFailureParams(i))
			}
			if e == nil {
				e = tx.Commit(t.Context())
			}
			results <- e
		})
	}
	close(start)
	wg.Wait()
	close(results)
	success, stale := 0, 0
	for e := range results {
		if e == nil {
			success++
		} else if errors.Is(e, pgx.ErrNoRows) {
			stale++
		} else {
			t.Fatal(e)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("settlements=%d stale=%d", success, stale)
	}
	c, err := q.LockComputer(t.Context(), db.LockComputerParams{EnvironmentID: i.EnvironmentID, ID: i.ComputerID})
	if err != nil {
		t.Fatal(err)
	}
	if c.PreparationAttemptCount != 3 || !c.NextPreparationAt.Valid {
		t.Fatalf("invalid backoff: %+v", c)
	}
	rows, err = q.ListFailedComputerPreparations(t.Context(), 10)
	if err != nil || len(rows) != 0 {
		t.Fatalf("settled rediscovery=%v %v", rows, err)
	}
}

func TestPreparationAllocationChargeRollbackAndReplay(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: instanceIDForLease(t, f, work)})
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL WHERE id=$1`, i.ID)
	charge := db.ChargeComputerPreparationParams{ComputerID: i.ComputerID, InstanceID: i.ID}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.New(tx).ChargeComputerPreparation(t.Context(), charge); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	c, err := db.New(f.Pool).LockComputer(t.Context(), db.LockComputerParams{EnvironmentID: i.EnvironmentID, ID: i.ComputerID})
	if err != nil {
		t.Fatal(err)
	}
	if c.PreparationAttemptCount != 0 || c.PreparationInstanceID.Valid {
		t.Fatal("rollback consumed attempt")
	}
	c, err = db.New(f.Pool).ChargeComputerPreparation(t.Context(), charge)
	if err != nil {
		t.Fatal(err)
	}
	if c.PreparationAttemptCount != 1 || c.PreparationInstanceID != i.ID || c.NextPreparationAt.Valid {
		t.Fatalf("charge=%+v", c)
	}
	if _, err = db.New(f.Pool).ChargeComputerPreparation(t.Context(), charge); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate charge=%v", err)
	}
	closePreparation(t, f, i)
	if _, err = db.New(f.Pool).ChargeComputerPreparation(t.Context(), charge); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unsettled failure reallocated=%v", err)
	}
}

func TestPreparationCompletesExactRecoveryAndRejectsStaleWriter(t *testing.T) {
	f, i := preparingComputer(t, 2)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET recovery_id=$2,recovery_disk_version_id=head_disk_version_id,recovery_reason='worker_lost',recovery_started_at=now(),writer_generation=writer_generation+1 WHERE id=$1`, i.ComputerID, pgvalue.NewUUIDv7())
	q := db.New(f.Pool)
	if _, err := q.CompleteComputerPreparation(t.Context(), completePreparationParams(i)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale writer completed recovery: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET writer_generation=$2 WHERE id=$1`, i.ComputerID, i.WriterGeneration)
	c, err := q.CompleteComputerPreparation(t.Context(), completePreparationParams(i))
	if err != nil {
		t.Fatal(err)
	}
	if !c.RecoveryCompletedAt.Valid || c.PreparationAttemptCount != 0 || c.PreparationInstanceID.Valid {
		t.Fatalf("recovery reset=%+v", c)
	}
}
