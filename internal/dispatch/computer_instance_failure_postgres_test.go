package dispatch

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestReconciliationSettlesDurableInstanceFailure(t *testing.T) {
	f, work, a := commandAssignmentFixture(t)
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
	failure := instanceFailure(i, computer.FailureRuntime, workerapi.RuntimeFailureReconcile, nil)
	if _, err = computer.RecordInstanceFailure(t.Context(), f.Pool, failure); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, work.RunID).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("physical reporter settled the Run: %s %v", status, err)
	}
	// No reporter runs between these transactions. A later reconciliation pass
	// must discover and settle the committed physical fact on its own.
	if _, err = a.ReconcileComputerInstances(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT status FROM runs WHERE id=$1`, work.RunID).Scan(&status); err != nil || status != "system_failed" {
		t.Fatalf("reconciled Run=%s %v", status, err)
	}
	if _, err = computer.RecordInstanceFailure(t.Context(), f.Pool, failure); !errors.Is(err, computer.ErrAuthorityChanged) {
		t.Fatalf("replayed physical failure=%v", err)
	}
}

// instanceObservation observes the Instance at its current fences.
func instanceObservation(i db.ComputerInstance) computer.Observation {
	return computer.Observation{
		Instance: computer.InstanceRef{
			Host: computer.Host{GroupID: pgvalue.MustUUIDValue(i.WorkerGroupID), HostID: pgvalue.MustUUIDValue(i.WorkerHostID), Epoch: i.WorkerEpoch},
			ID:   pgvalue.MustUUIDValue(i.ID), DesiredVersion: i.DesiredVersion,
		},
		ExpectedObservedVersion: i.ObservedVersion,
	}
}

// instanceFailure reports the Instance failed at its current fences.
func instanceFailure(i db.ComputerInstance, kind computer.FailureKind, reason string, detail []byte) computer.Failure {
	return computer.Failure{Observation: instanceObservation(i), Kind: kind, Reason: reason, Error: detail}
}

func TestSourceFailureReconcilesPendingMembersAfterReporterStops(t *testing.T) {
	f, work, a := commandAssignmentFixture(t)
	pending := pendingSharedCommand(t, f, work)
	peer := pendingSharedCommand(t, f, work)
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
	if _, err = computer.RecordInstanceFailure(t.Context(), f.Pool, instanceFailure(i, computer.FailureSourceUnavailable, workerapi.RuntimeFailureComputerSource, []byte(`{"code":"source_missing"}`))); err != nil {
		t.Fatal(err)
	}
	var untouched bool
	if err = f.Pool.QueryRow(t.Context(), `SELECT r.status='queued' AND p.status='pending' FROM runs r JOIN computer_commands p ON p.id=$2 WHERE r.id=$1`, work.RunID, pending.CommandID).Scan(&untouched); err != nil || !untouched {
		t.Fatalf("reporter settled logical members: %v %v", untouched, err)
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
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,terminal_reason_code FROM computer_commands WHERE id=$1`, peer.CommandID).Scan(&status, &code); err != nil || status != "failed" || code != "computer_source_unavailable" {
		t.Fatalf("peer Command=%s %s %v", status, code, err)
	}
}
