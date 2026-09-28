package dispatch

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestInstanceFailureIsDurableWithoutLogicalSettlement(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL WHERE id=$1`, id)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=8,preparation_instance_id=$1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, id)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET current_run_lease_id=NULL,status='queued' WHERE id=$1`, work.RunID)
	q := db.New(f.Pool)
	i, err := q.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	params := db.MarkComputerInstanceFailedParams{ID: i.ID, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureReconcile)}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	failed, err := RecordComputerInstanceFailure(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), params)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if failed.ObservedState != "failed" || failed.DesiredState != "closed" || failed.ReclaimedAt.Valid || failed.ReservedCPUMillis != i.ReservedCPUMillis {
		t.Fatalf("failure released capacity: %+v", failed)
	}
	var count int
	var settled bool
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT preparation_attempt_count,preparation_failure IS NOT NULL,(SELECT status FROM runs WHERE id=$2) FROM computers WHERE id=$1`, i.ComputerID, work.RunID).Scan(&count, &settled, &status); err != nil {
		t.Fatal(err)
	}
	if count != 8 || settled || status != "queued" {
		t.Fatalf("physical reporter settled logical work: %d/%v/%s", count, settled, status)
	}
	// No reporter runs between these transactions. A later reconciliation pass
	// must discover and settle the committed physical fact on its own.
	if _, err = a.ReconcileComputerInstances(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, work.RunID).Scan(&status); err != nil || status != "system_failed" {
		t.Fatalf("reconciled Run=%s %v", status, err)
	}
	tx, err = f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err = RecordComputerInstanceFailure(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("replayed physical failure=%v", err)
	}
}

func TestInstanceFailureRejectsForeignWorkerGroup(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = RecordComputerInstanceFailure(t.Context(), tx, uuid.NewV7(), db.MarkComputerInstanceFailedParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureWorkerInvalid)})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign Worker Group=%v", err)
	}
}

func TestInstanceReclaimRequiresCurrentCloseAndKeepsFailure(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	reclaim := func(params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
		tx, e := f.Pool.Begin(t.Context())
		if e != nil {
			return db.ComputerInstance{}, e
		}
		defer tx.Rollback(context.Background())
		row, e := RecordComputerInstanceReclaim(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), params)
		if e != nil {
			return row, e
		}
		return row, tx.Commit(t.Context())
	}
	params := db.ReclaimComputerInstanceParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, RequireFailure: true, Reason: pgvalue.Text("cleanup_complete"), Evidence: []byte(`{"method":"host_reconciled"}`)}
	if _, err = reclaim(params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("live instance reclaimed: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed' WHERE id=$1`, id)
	i.DesiredVersion++
	params.DesiredVersion = i.DesiredVersion
	if _, err = reclaim(params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unreported failure reclaimed: %v", err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	failed, err := RecordComputerInstanceFailure(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), db.MarkComputerInstanceFailedParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureReconcile), Error: []byte(`{"code":"test_failure"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = reclaim(params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("old observation reclaimed: %v", err)
	}

	var processEmpty bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&processEmpty); err != nil || !processEmpty {
		t.Fatalf("failure without physical proof reconciled process: %v %v", processEmpty, err)
	}
	params.DesiredVersion = failed.DesiredVersion
	params.ExpectedObservedVersion = failed.ObservedVersion
	reclaimed, err := reclaim(params)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed.ReclaimedAt.Valid || reclaimed.MountState != "unmounted" || reclaimed.ObservedState != "failed" || reclaimed.TerminalReasonCode != failed.TerminalReasonCode || string(reclaimed.TerminalError) != string(failed.TerminalError) {
		t.Fatalf("lost failure diagnosis: %+v", reclaimed)
	}

	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at=$2 FROM run_leases WHERE id=$1`, work.LeaseID, reclaimed.ReclaimedAt).Scan(&processEmpty); err != nil || !processEmpty {
		t.Fatalf("physical exclusion left member unreconciled: %v %v", processEmpty, err)
	}
	replayed, err := reclaim(params)
	if err != nil || replayed.ObservedVersion != reclaimed.ObservedVersion || replayed.ReclaimedAt != reclaimed.ReclaimedAt || string(replayed.ReclaimEvidence) != string(reclaimed.ReclaimEvidence) {
		t.Fatalf("cleanup replay changed receipt: %+v / %v", replayed, err)
	}
	stale := params
	stale.DesiredVersion--
	if _, err := reclaim(stale); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("stale close intent replay=%v", err)
	}
	var live bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT current_run_lease_id=$2 FROM runs WHERE id=$1`, work.RunID, work.LeaseID).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if !live {
		t.Fatal("physical reclaim settled logical Run directly")
	}
}

func TestSourceFailureReconcilesPendingMembersAfterReporterStops(t *testing.T) {
	f, work, a := commandPlacementFixture(t)
	pending := pendingSharedCommand(t, f, work)
	owned := pendingSharedCommand(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_commands SET created_by_subject_type='api_key' WHERE id=$1`, pending.CommandID)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='allocated',observed_desired_version=0,ready_at=NULL WHERE id=$1`, id)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=1,preparation_instance_id=$1 WHERE id=(SELECT computer_id FROM computer_instances WHERE id=$1)`, id)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET current_run_lease_id=NULL,status='queued' WHERE id=$1`, work.RunID)
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = RecordComputerInstanceFailure(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), db.MarkComputerInstanceFailedParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureComputerSource), Error: []byte(`{"code":"source_missing"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT c.preparation_attempt_count=1 AND c.preparation_failure IS NULL AND c.recovery_failure IS NOT NULL AND r.status='queued' AND p.status='pending' AND c.head_disk_version_id=i.source_disk_version_id AND i.reclaimed_at IS NULL FROM computers c JOIN computer_instances i ON i.computer_id=c.id JOIN runs r ON r.id=$2 JOIN computer_commands p ON p.id=$3 WHERE i.id=$1`, id, work.RunID, pending.CommandID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("reporter changed logical ownership or lost source: %v %v", untouched, err)
	}
	for range 2 {
		if _, err = a.ReconcileComputerInstances(t.Context(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var status, code string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,failure->>'code' FROM runs WHERE id=$1`, work.RunID).Scan(&status, &code); err != nil || status != "system_failed" || code != "computer_source_unavailable" {
		t.Fatalf("Run=%s %s %v", status, code, err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT status,terminal_reason_code FROM computer_commands WHERE id=$1`, pending.CommandID).Scan(&status, &code); err != nil || status != "failed" || code != "computer_source_unavailable" {
		t.Fatalf("Command=%s %s %v", status, code, err)
	}
	var ownedCancelled bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='cancelled' AND terminal_reason_code='computer_command_cancelled' FROM computer_commands WHERE id=$1`, owned.CommandID).Scan(&ownedCancelled); err != nil || !ownedCancelled {
		t.Fatalf("owned cancellation=%v %v", ownedCancelled, err)
	}

}

func TestFatalInstanceFailureDrainsWorkerWithoutReclaim(t *testing.T) {
	f, work, _ := commandPlacementFixture(t)
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	_, err = RecordComputerInstanceFailure(t.Context(), tx, pgvalue.MustUUIDValue(i.WorkerGroupID), db.MarkComputerInstanceFailedParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, ReasonCode: pgvalue.Text(workerapi.RuntimeFailureWorkerInvalid)})
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT h.status='draining' AND h.current_epoch=i.worker_epoch AND i.reclaimed_at IS NULL FROM worker_hosts h JOIN computer_instances i ON i.worker_host_id=h.id WHERE i.id=$1`, id).Scan(&valid); err != nil || !valid {
		t.Fatalf("fatal worker remained eligible or was reclaimed: %v %v", valid, err)
	}
}
