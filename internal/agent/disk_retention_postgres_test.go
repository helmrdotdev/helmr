package agent

import (
	"context"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestDiskRetentionPreservesWriterUncertaintyAndRecoveryHead(t *testing.T) {
	f := newFixture(t)
	s := newSaveStorageFixture(t, f)
	a, old := f.finalize(t, "old-result")
	oldRoot, oldIdentity := s.cut(t, 17)
	f.capture(t, old, oldIdentity)
	if err := s.publish(t, old.ID, oldRoot); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, a.TurnID); err != nil {
		t.Fatal(err)
	}
	b, head := f.finalize(t, "current-result")
	headRoot, headIdentity := s.cut(t, 18)
	f.capture(t, head, headIdentity)
	if err := s.publish(t, head.ID, headRoot); err != nil {
		t.Fatal(err)
	}
	if err := Complete(t.Context(), f.pool, f.env, f.session, b.TurnID); err != nil {
		t.Fatal(err)
	}
	_, pending := f.finalize(t, "uncertain-result")
	pendingRoot, pendingIdentity := s.cut(t, 19)
	f.capture(t, pending, pendingIdentity)
	s.certify(t, s.ref(pending.ID), pendingRoot)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET expires_at=clock_timestamp()-interval '1 second',status='lost' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	reconcile := func() {
		t.Helper()
		if err := ReconcileComputerDiskRetention(t.Context(), f.pool); err != nil {
			t.Fatal(err)
		}
	}
	assertState := func(oldRetired bool) {
		t.Helper()
		var correct bool
		if err := f.pool.QueryRow(t.Context(), `SELECT
   (old.root_id IS NULL)=$5 AND (old.payload_retired_at IS NOT NULL)=$5
   AND head.root_id IS NOT NULL AND c.recovery_save_id=head.id
   AND l.disk_released_at IS NULL AND pending.status='captured'
   AND EXISTS(SELECT 1 FROM computer_object_pins p WHERE p.environment_id=c.environment_id AND p.save_id=pending.id)
   AND t.status='completed' AND t.completion_save_id=old.id
   FROM computers c JOIN computer_leases l ON l.environment_id=c.environment_id AND l.computer_id=c.id
   JOIN computer_saves old ON old.environment_id=c.environment_id AND old.id=$2
   JOIN computer_saves head ON head.environment_id=c.environment_id AND head.id=$3
   JOIN computer_saves pending ON pending.environment_id=c.environment_id AND pending.id=$4
   JOIN turns t ON t.environment_id=c.environment_id AND t.id=old.turn_id
   WHERE c.environment_id=$1 AND c.id=$6`, f.env, old.ID, head.ID, pending.ID, oldRetired, f.computer).Scan(&correct); err != nil || !correct {
			t.Fatalf("retention state retired=%v: valid=%v error=%v", oldRetired, correct, err)
		}
	}
	reconcile()
	assertState(false)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET fenced_at=clock_timestamp(),fence_evidence='all source work stopped' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	reconcile()
	assertState(true)
	reconcile()
	assertState(true)
	if err := s.publisher.Publish(t.Context(), s.ref(old.ID), oldRoot, "receipt retry after retirement"); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileSavePublication(t.Context(), f.pool, f.env, old.ID, oldIdentity); err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_saves SET status='failed',failure_evidence='source proved no publication' WHERE environment_id=$1 AND id=$2`, f.env, pending.ID)
	reconcile()
	var released bool
	if err := f.pool.QueryRow(t.Context(), `SELECT disk_released_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer).Scan(&released); err != nil || !released {
		t.Fatalf("settled source retained lease: %v %v", released, err)
	}

	for range 8 {
		reconcile()
	}
	var graphCollected, headRetained, membershipCollected bool
	if err := f.pool.QueryRow(t.Context(), `SELECT
 NOT EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$2),
 EXISTS(SELECT 1 FROM computer_objects WHERE environment_id=$1 AND digest=$3),
 NOT EXISTS(SELECT 1 FROM cas_objects WHERE digest=$2)`, f.env, oldRoot.Pack.Digest, headRoot.Pack.Digest).Scan(&graphCollected, &headRetained, &membershipCollected); err != nil || !graphCollected || !headRetained || !membershipCollected {
		t.Fatalf("graph collection=%v head=%v membership collected=%v error=%v", graphCollected, headRetained, membershipCollected, err)
	}
}

func TestDiskRetentionKeepsPendingCheckpointSource(t *testing.T) {
	f := readyCheckpointFixture(t)
	dbtest.MustExec(t, t.Context(), f.f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='source stopped' WHERE environment_id=$1 AND computer_id=$2`, f.f.env, f.f.computer)
	if err := ReconcileComputerDiskRetention(t.Context(), f.f.pool); err != nil {
		t.Fatal(err)
	}
	var retained bool
	if err := f.f.pool.QueryRow(t.Context(), `SELECT disk_released_at IS NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.f.env, f.f.computer).Scan(&retained); err != nil || !retained {
		t.Fatalf("checkpoint source lost: %v %v", retained, err)
	}
}

func TestDiskRetentionSkipsLockedOwnerBeforeBatchBound(t *testing.T) {
	f := newFixture(t)
	newSaveStorageFixture(t, f)
	dbtest.MustExec(t, t.Context(), f.pool, `UPDATE computer_leases SET status='released',fenced_at=clock_timestamp(),fence_evidence='source stopped' WHERE environment_id=$1 AND computer_id=$2`, f.env, f.computer)
	const copyLease = `INSERT INTO computer_leases(environment_id,computer_id,epoch,worker_host_id,worker_epoch,expires_at,delivered_at,status,computer_instance_id,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,base_root_id,write_key_id,fenced_at,fence_evidence)
 SELECT environment_id,$3,$4,worker_host_id,worker_epoch,expires_at,delivered_at,'released',$5,channel_credential_digest,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,base_root_id,COALESCE($6::uuid,write_key_id),fenced_at,fence_evidence FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=1`
	for epoch := 2; epoch <= 101; epoch++ {
		dbtest.MustExec(t, t.Context(), f.pool, copyLease, f.env, f.computer, f.computer, epoch, uuid.NewV7(), nil)
	}
	later, key := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computers(environment_id,id,initial_root_id,initial_root_digest) SELECT environment_id,$3,initial_root_id,initial_root_digest FROM computers WHERE environment_id=$1 AND id=$2`, f.env, f.computer, later)
	dbtest.MustExec(t, t.Context(), f.pool, `INSERT INTO computer_data_keys(id,environment_id,writer_computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'test',decode('01','hex'))`, key, f.env, later)
	dbtest.MustExec(t, t.Context(), f.pool, copyLease, f.env, f.computer, later, 1, uuid.NewV7(), key)
	tx, err := f.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(t.Context(), `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, f.env, f.computer); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := ReconcileComputerDiskRetention(ctx, f.pool); err != nil {
		t.Fatal(err)
	}
	var released bool
	if err := f.pool.QueryRow(t.Context(), `SELECT disk_released_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2`, f.env, later).Scan(&released); err != nil || !released {
		t.Fatalf("later owner blocked by locked prefix: %v %v", released, err)
	}
	var earlyReleased int
	if err := f.pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND disk_released_at IS NOT NULL`, f.env, f.computer).Scan(&earlyReleased); err != nil || earlyReleased != 0 {
		t.Fatalf("locked owner changed: %d %v", earlyReleased, err)
	}
}
