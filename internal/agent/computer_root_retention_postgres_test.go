package agent

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestDeletedComputerRootsWaitForPhysicalClosure(t *testing.T) {
	f := newAdmissionFixture(t)
	s := newSaveStorageFixture(t, f)
	admission, save := f.finalize(t, "completed-before-delete")
	root, identity := s.cut(t, 90)
	f.capture(t, save, identity)
	if err := s.publish(t, save.ID, root); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, admission.TurnID); err != nil {
		t.Fatal(err)
	}
	// Deletion may be accepted after the Session process ends while its physical
	// Computer is still shutting down. Fixture that accepted, unreconciled state.
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2;
 UPDATE sessions SET status='closed' WHERE environment_id=$1 AND computer_id=$2;
 UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2;
 UPDATE computer_leases SET status='releasing' WHERE environment_id=$1 AND computer_id=$2`, pgx.QueryExecModeSimpleProtocol, f.env, f.computer)
	if err := ReconcileComputerDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.pool.QueryRow(t.Context(), `SELECT initial_root_id IS NOT NULL AND recovery_save_id=$3 AND storage_reservation_bytes IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer, save.ID).Scan(&retained); err != nil || !retained {
		t.Fatalf("accepted deletion lost physical source: %v %v", retained, err)
	}
	if err := ObserveComputerStopped(t.Context(), f.pool, *f.host(), ComputerLeaseIdentity{EnvironmentID: f.env, ComputerID: f.computer, InstanceID: f.computer, Epoch: 1}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `SELECT id FROM computer_saves WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, save.ID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileComputerDiskRetention(t.Context(), f.pool); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(t.Context(), `SELECT initial_root_id IS NULL AND recovery_save_id IS NULL AND storage_reservation_bytes IS NOT NULL FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer).Scan(&retained); err != nil || !retained {
		t.Fatalf("pending save cleanup released reservation: %v %v", retained, err)
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if err := ReconcileComputerDiskRetention(t.Context(), f.pool); err != nil {
			t.Fatal(err)
		}
	}
	var retired, receipt bool
	if err := f.pool.QueryRow(t.Context(), `SELECT c.initial_root_id IS NULL AND c.initial_payload_retired_at IS NOT NULL AND c.recovery_save_id IS NULL AND octet_length(c.initial_root_digest)=32 AND c.storage_reservation_bytes IS NULL,
 s.root_id IS NULL AND s.payload_retired_at IS NOT NULL AND t.status='completed' AND t.completion_save_id=s.id
 FROM computers c JOIN computer_saves s ON s.environment_id=c.environment_id AND s.id=$3 JOIN turns t ON t.environment_id=c.environment_id AND t.id=s.turn_id WHERE c.environment_id=$1 AND c.id=$2`, f.env, f.computer, save.ID).Scan(&retired, &receipt); err != nil || !retired || !receipt {
		t.Fatalf("roots retired=%v receipt=%v error=%v", retired, receipt, err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, save.ID, identity); err != nil {
		t.Fatal(err)
	}
}
