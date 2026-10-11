package agent

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestCheckpointRetentionRequiresBothPhysicalClosures(t *testing.T) {
	f := newComputerRestoreFixture(t)
	f.prepare(t)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='logical target loss' WHERE environment_id=$1 AND id=$2`, f.f.env, f.manifest.CheckpointID)
	assertObjects := func(count int) {
		t.Helper()
		var got int
		if err := f.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2`, f.f.env, f.manifest.CheckpointID).Scan(&got); err != nil || got != count {
			t.Fatalf("checkpoint objects=%d want=%d error=%v", got, count, err)
		}
	}
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	assertObjects(4)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='target physical closure' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, f.f.env, f.f.computer, f.epoch)
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	assertObjects(0)
	var retained bool
	if err := f.f.pool.QueryRow(t.Context(), `SELECT k.manifest IS NOT NULL AND octet_length(k.capture_digest)=32 AND k.status='lost' AND (SELECT count(*) FROM computer_checkpoint_members m WHERE m.environment_id=k.environment_id AND m.checkpoint_id=k.id)=2 FROM computer_checkpoints k WHERE environment_id=$1 AND id=$2`, f.f.env, f.manifest.CheckpointID).Scan(&retained); err != nil || !retained {
		t.Fatalf("checkpoint identity lost: %v %v", retained, err)
	}
}

func TestCheckpointRetentionKeepsUnreconciledConsumedImage(t *testing.T) {
	f := newComputerRestoreFixture(t)
	installation := f.prepare(t)
	if err := ValidateComputerRestore(t.Context(), f.f.pool, f.host, f.f.env, installation, restoreReceipt(installation, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(t.Context(), f.f.pool, f.host, f.f.env, installation, restoreReceipt(installation, true, false)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='target physical closure' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, f.f.env, f.f.computer, f.epoch)
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2`, f.f.env, f.manifest.CheckpointID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("unreconciled consumed image removed: %d %v", count, err)
	}
}

func TestCheckpointRetentionDoesNotTreatLostSourceAsStopped(t *testing.T) {
	f := readyCheckpointFixture(t)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_checkpoints SET status='lost',capture_request=NULL,terminal_evidence='logical source loss' WHERE environment_id=$1 AND id=$2`, f.f.env, f.manifest.CheckpointID)
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2`, f.f.env, f.manifest.CheckpointID).Scan(&count); err != nil || count != 4 {
		t.Fatalf("unfenced source image removed: %d %v", count, err)
	}
}

func TestCheckpointRetentionPreservesConsumedManifestAfterUnpinning(t *testing.T) {
	f := newComputerRestoreFixture(t)
	p := f.prepare(t)
	ctx := t.Context()
	if err := ValidateComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, false, false)); err != nil {
		t.Fatal(err)
	}
	if err := CommitComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, false)); err != nil {
		t.Fatal(err)
	}
	acknowledgeComputerMembers(t, f.f, f.host, p)
	if err := CompleteComputerRestore(ctx, f.f.pool, f.host, f.f.env, p, restoreReceipt(p, true, true)); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, ctx, f.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='target physical closure' WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, f.f.env, f.f.computer, f.epoch)
	if err := ReconcileComputerDiskRetention(ctx, f.f.pool); err != nil {
		t.Fatal(err)
	}
	expected, err := f.manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var retained bool
	err = f.f.pool.QueryRow(ctx, `SELECT k.status='consumed' AND k.controls_reconciled_at IS NOT NULL AND k.manifest=$3
  AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_objects o WHERE o.environment_id=k.environment_id AND o.checkpoint_id=k.id)
  AND (SELECT count(*)=2 FROM computer_checkpoint_members m WHERE m.environment_id=k.environment_id AND m.checkpoint_id=k.id)
  FROM computer_checkpoints k WHERE environment_id=$1 AND id=$2`, f.f.env, f.manifest.CheckpointID, expected).Scan(&retained)
	if err != nil || !retained {
		t.Fatalf("consumed checkpoint identity after unpinning: %v %v", retained, err)
	}
}
