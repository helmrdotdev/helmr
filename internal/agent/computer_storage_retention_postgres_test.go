package agent

import (
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/jackc/pgx/v5"
)

func TestDeletedComputerStorageWaitsForCheckpointCleanup(t *testing.T) {
	f := checkpointStorageForFixture(t, newAdmissionFixture(t))
	if err := f.publisher.RegisterCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	f.upload(t)
	if err := f.publish(t, f.save, f.manifest.Disk); err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.CompleteCheckpoint(t.Context(), f.ref, f.manifest); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE session_processes SET status='stopped',fenced_at=clock_timestamp() WHERE environment_id=$1;
 UPDATE sessions SET status='closed' WHERE environment_id=$1;
 UPDATE computers SET deleted_at=clock_timestamp() WHERE environment_id=$1 AND id=$2;
 UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='Computer deletion retired continuation' WHERE environment_id=$1;
 UPDATE computer_leases SET status='releasing' WHERE environment_id=$1`, pgx.QueryExecModeSimpleProtocol, f.f.env, f.f.computer)
	if err := ObserveComputerStopped(t.Context(), f.f.pool, *f.f.host(), ComputerLeaseIdentity{EnvironmentID: f.f.env, ComputerID: f.f.computer, InstanceID: f.f.computer, Epoch: 1}, uuid.Nil()); err != nil {
		t.Fatal(err)
	}
	blocker, err := f.f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(t.Context())
	if _, err := blocker.Exec(t.Context(), `SELECT id FROM computer_checkpoints WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.f.env, f.manifest.CheckpointID); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.f.pool.QueryRow(t.Context(), `SELECT c.initial_root_id IS NULL AND c.recovery_save_id IS NULL AND c.storage_reservation_bytes IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.root_id IS NOT NULL)
 AND EXISTS(SELECT 1 FROM computer_checkpoint_objects o WHERE o.environment_id=c.environment_id AND o.checkpoint_id=$3)
 FROM computers c WHERE c.environment_id=$1 AND c.id=$2`, f.f.env, f.f.computer, f.manifest.CheckpointID).Scan(&retained); err != nil || !retained {
		t.Fatalf("checkpoint cleanup returned capacity early: %v %v", retained, err)
	}
	if err := blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var released bool
	if err := f.f.pool.QueryRow(t.Context(), `SELECT c.storage_reservation_bytes IS NULL
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_objects o WHERE o.environment_id=c.environment_id AND o.checkpoint_id=$3)
 AND EXISTS(SELECT 1 FROM computer_checkpoints k WHERE k.environment_id=c.environment_id AND k.id=$3 AND k.manifest IS NOT NULL)
 FROM computers c WHERE c.environment_id=$1 AND c.id=$2`, f.f.env, f.f.computer, f.manifest.CheckpointID).Scan(&released); err != nil || !released {
		t.Fatalf("checkpoint storage release or history failed: %v %v", released, err)
	}
}
