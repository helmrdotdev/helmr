package dispatch

import (
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
)

func TestPreparationReconcilerSettlesUnstartedMembers(t *testing.T) {
	f, work, a := commandAssignmentFixture(t)
	pending := pendingSharedCommand(t, f, work)
	peer := pendingSharedCommand(t, f, work)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET preparation_attempt_count=8,preparation_instance_id=(SELECT computer_instance_id FROM run_leases WHERE id=$1) WHERE id=(SELECT computer_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	// Ready acknowledgement alone does not imply a successful restored activation.
	// Simulate a durable close before the preparation success transaction.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,admission_state='closed' WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runs SET current_run_lease_id=NULL,status='queued' WHERE id=$1`, work.RunID)
	for range 2 {
		if _, err := a.ReconcileComputerInstances(t.Context(), 10); err != nil {
			t.Fatal(err)
		}
	}
	var status, code string
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,terminal_reason_code FROM computer_commands WHERE id=$1`, pending.CommandID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || code != "computer_preparation_exhausted" {
		t.Fatalf("Command=%s %s", status, code)
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,failure->>'code' FROM runs WHERE id=$1`, work.RunID).Scan(&status, &code); err != nil {
		t.Fatal(err)
	}
	if status != "system_failed" || code != "computer_preparation_exhausted" {
		t.Fatalf("Run=%s %s", status, code)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT reclaimed_at IS NULL AND reserved_cpu_millis>0 FROM computer_instances WHERE id=(SELECT computer_instance_id FROM run_leases WHERE id=$1)`, work.LeaseID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !retained {
		t.Fatal("logical failure released physical reservation")
	}
	if err := f.Pool.QueryRow(t.Context(), `SELECT status,terminal_reason_code FROM computer_commands WHERE id=$1`, peer.CommandID).Scan(&status, &code); err != nil || status != "failed" || code != "computer_preparation_exhausted" {
		t.Fatalf("peer Command=%s %s %v", status, code, err)
	}

}
