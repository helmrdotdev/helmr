package computer

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// RecordCheckpointManifest pins the manifest's objects and stores the
// manifest on its checkpoint. The caller holds the capture locks.
func RecordCheckpointManifest(ctx context.Context, tx pgx.Tx, env uuid.UUID, manifest computercheckpoint.Manifest, raw []byte) error {
	for _, o := range manifest.Objects() {
		if _, err := tx.Exec(ctx, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,$2) ON CONFLICT DO NOTHING`, o.Digest, o.SizeBytes); err != nil {
			return err
		}
		var size int64
		var available bool
		if err := tx.QueryRow(ctx, `SELECT size_bytes,not_retired FROM cas_blobs WHERE digest=$1 FOR KEY SHARE`, o.Digest).Scan(&size, &available); err != nil {
			return err
		}
		if size != o.SizeBytes || !available {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `INSERT INTO checkpoint_objects(environment_id,checkpoint_id,role,digest,size_bytes,media_type) VALUES($1,$2,$3,$4,$5,$6)`, env, manifest.CheckpointID, o.Role, o.Digest, o.SizeBytes, o.MediaType); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET manifest=$3,vm_platform_id=$4 WHERE environment_id=$1 AND id=$2`, env, manifest.CheckpointID, raw, manifest.Runtime.RuntimeID)
	return err
}

// MarkCheckpointReady records a checkpoint whose registered objects are
// stored as ready. The caller holds the capture locks and has verified the
// source lease and Secret authority.
func MarkCheckpointReady(ctx context.Context, tx pgx.Tx, env uuid.UUID, manifest computercheckpoint.Manifest) error {
	var org uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT org_id FROM environments WHERE id=$1`, env).Scan(&org); err != nil {
		return err
	}
	// Typed pins must still be present with their exact registered descriptors.
	for _, o := range manifest.Objects() {
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM checkpoint_objects WHERE environment_id=$1 AND checkpoint_id=$2 AND role=$3 AND digest=$4 AND size_bytes=$5 AND media_type=$6)`, env, manifest.CheckpointID, o.Role, o.Digest, o.SizeBytes, o.MediaType).Scan(&pinned); err != nil {
			return err
		}
		if !pinned {
			return ErrConflict
		}
		if _, err := db.New(tx).UpsertCasObject(ctx, db.UpsertCasObjectParams{OrgID: pgvalue.UUID(org), Digest: o.Digest, SizeBytes: o.SizeBytes, MediaType: o.MediaType}); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE checkpoints SET status='ready',ready_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, manifest.CheckpointID)
	return err
}

// ValidateCheckpointRuntime matches the host platform recorded for the checkpoint.
func ValidateCheckpointRuntime(ctx context.Context, tx pgx.Tx, host workergroup.HostPrincipal, r vm.CheckpointIdentity) error {
	if r.RuntimeBackend != "firecracker" || !sha256sum.ValidDigest(r.VMConfigDigest) || r.VMVCPUCount <= 0 {
		return ErrConflict
	}
	var valid bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts h JOIN vm_platforms p ON p.id=h.vm_platform_id JOIN worker_pool_cpu_shapes s ON s.worker_pool_id=h.worker_pool_id WHERE h.id=$1 AND p.id=$2 AND p.kernel_digest=$3 AND p.initramfs_digest=$4 AND p.rootfs_digest=$5 AND s.vcpu_count=$6 AND s.cpu_config_digest=$7 AND h.per_vm_cpu_millis >= $6::bigint*1000)`, host.HostID, r.RuntimeID, r.KernelDigest, r.InitramfsDigest, r.RootfsDigest, r.VMVCPUCount, r.CPUConfigDigest).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return ErrConflict
	}
	return nil
}
