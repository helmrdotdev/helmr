package computer

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// runningInstance is the ready Instance of a running runtest lease.
func runningInstance(t *testing.T) (runtest.Fixture, runtest.RunLease, db.ComputerInstance) {
	t.Helper()
	f := runtest.New(t)
	work := f.AddRunLease(t, "running", time.Now().Add(-time.Minute))
	var id pgtype.UUID
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_instance_id FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	i, err := db.New(f.Pool).GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: pgvalue.UUID(f.EnvironmentID), ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return f, work, i
}

// observationOf observes the Instance at its current desired and observed
// versions on its worker epoch.
func observationOf(i db.ComputerInstance) Observation {
	return Observation{
		Instance: InstanceRef{
			Host: Host{GroupID: pgvalue.MustUUIDValue(i.WorkerGroupID), HostID: pgvalue.MustUUIDValue(i.WorkerHostID), Epoch: i.WorkerEpoch},
			ID:   pgvalue.MustUUIDValue(i.ID), DesiredVersion: i.DesiredVersion,
		},
		ExpectedObservedVersion: i.ObservedVersion,
	}
}

func applyFailure(t *testing.T, f runtest.Fixture, failure Failure) (db.ComputerInstance, error) {
	t.Helper()
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	row, err := recordFailure(t.Context(), tx, failure)
	if err != nil {
		return row, err
	}
	return row, tx.Commit(t.Context())
}

func TestInstanceFailureRejectsForeignWorkerGroup(t *testing.T) {
	f, _, i := runningInstance(t)
	observation := observationOf(i)
	observation.Instance.Host.GroupID = uuid.NewV7()
	if _, err := applyFailure(t, f, Failure{Observation: observation, Kind: FailureWorkerInvalid, Reason: "worker_runtime_invalid"}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign Worker Group=%v", err)
	}
}

func TestInstanceReclaimRequiresCurrentCloseAndKeepsFailure(t *testing.T) {
	f, work, i := runningInstance(t)
	id := i.ID
	reclaim := func(params db.ReclaimComputerInstanceParams) (db.ComputerInstance, error) {
		return reclaimInstance(t.Context(), f.Pool, pgvalue.MustUUIDValue(i.WorkerGroupID), params)
	}
	params := db.ReclaimComputerInstanceParams{ID: id, WorkerHostID: i.WorkerHostID, WorkerEpoch: i.WorkerEpoch, DesiredVersion: i.DesiredVersion, ExpectedObservedVersion: i.ObservedVersion, RequireFailure: true, Reason: pgvalue.Text("cleanup_complete"), Evidence: []byte(`{"method":"host_reconciled"}`)}
	if _, err := reclaim(params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("live instance reclaimed: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed' WHERE id=$1`, id)
	i.DesiredVersion++
	params.DesiredVersion = i.DesiredVersion
	if _, err := reclaim(params); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("unreported failure reclaimed: %v", err)
	}
	failed, err := applyFailure(t, f, Failure{Observation: observationOf(i), Kind: FailureInstance, Reason: "instance_reconcile_failed", Error: []byte(`{"code":"test_failure"}`)})
	if err != nil {
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

func TestFatalInstanceFailureDrainsWorkerWithoutReclaim(t *testing.T) {
	f, _, i := runningInstance(t)
	if _, err := applyFailure(t, f, Failure{Observation: observationOf(i), Kind: FailureWorkerInvalid, Reason: "worker_runtime_invalid"}); err != nil {
		t.Fatal(err)
	}
	var valid bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT h.status='draining' AND h.current_epoch=i.worker_epoch AND i.reclaimed_at IS NULL FROM worker_hosts h JOIN computer_instances i ON i.worker_host_id=h.id WHERE i.id=$1`, i.ID).Scan(&valid); err != nil || !valid {
		t.Fatalf("fatal worker remained eligible or was reclaimed: %v %v", valid, err)
	}
}

// A failure report of an invalid worker epoch drains the host before any
// Instance fence, even when the report's cleanup proof is invalid.
func TestWorkerInvalidFailureDrainsBeforeValidatingCleanupProof(t *testing.T) {
	f, _, i := runningInstance(t)
	_, err := RecordInstanceFailure(t.Context(), f.Pool, Failure{Observation: observationOf(i), Kind: FailureWorkerInvalid, Reason: "worker_runtime_invalid", CleanupProof: &CleanupProof{Method: "assumed", CompletedAt: time.Now()}})
	var input InputError
	if !errors.As(err, &input) {
		t.Fatalf("invalid cleanup proof=%v", err)
	}
	var draining bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT status='draining' FROM worker_hosts WHERE id=$1`, i.WorkerHostID).Scan(&draining); err != nil || !draining {
		t.Fatalf("invalid epoch not drained: %v %v", draining, err)
	}
}

// A failure report with a cleanup proof fails and reclaims the Instance in
// separate transactions.
func TestFailureWithCleanupProofReclaimsAndReplays(t *testing.T) {
	f, work, i := runningInstance(t)
	failure := Failure{Observation: observationOf(i), Kind: FailureInstance, Reason: "instance_reconcile_failed", Error: []byte(`{}`), CleanupProof: &CleanupProof{Method: CleanupNotMaterialized, CompletedAt: time.Now()}}
	reclaimed, err := RecordInstanceFailure(t.Context(), f.Pool, failure)
	if err != nil {
		t.Fatal(err)
	}
	if !reclaimed.ReclaimedAt.Valid || reclaimed.ObservedState != "failed" || reclaimed.TerminalReasonCode.String != "instance_reconcile_failed" || string(reclaimed.ReclaimEvidence) == "" {
		t.Fatalf("failed Instance not reclaimed: %+v", reclaimed)
	}
	var reconciled bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT process_reconciled_at IS NOT NULL FROM run_leases WHERE id=$1`, work.LeaseID).Scan(&reconciled); err != nil || !reconciled {
		t.Fatalf("reclaim left processes unreconciled: %v %v", reconciled, err)
	}
}

func TestClosedObservationRequiresPhysicalTeardownProof(t *testing.T) {
	f, _, i := runningInstance(t)
	for _, closure := range []Closure{
		{Observation: observationOf(i), Reason: "closed"},
		{Observation: observationOf(i), Reason: "closed", CleanupProof: &CleanupProof{Method: CleanupNotMaterialized, CompletedAt: time.Now()}},
	} {
		var input InputError
		if _, err := RecordInstanceClosed(t.Context(), f.Pool, closure); !errors.As(err, &input) {
			t.Fatalf("closure without teardown proof=%v", err)
		}
	}
	if _, err := RecordInstanceClosed(t.Context(), f.Pool, Closure{Observation: observationOf(i), Reason: "closed", CleanupProof: &CleanupProof{Method: CleanupMachineClosed, CompletedAt: time.Now()}}); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("closure of a ready Instance=%v", err)
	}
}
