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

// PublishCheckpointObjects records storage-verified objects under their owning
// organization. The caller holds the capture locks and must recheck authority
// after these potentially blocking writes before marking the checkpoint ready.
func PublishCheckpointObjects(ctx context.Context, tx pgx.Tx, env uuid.UUID, manifest computercheckpoint.Manifest) error {
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
	return nil
}

// MarkCheckpointReady records readiness after object publication and the final
// authority check. The caller holds the capture locks.
func MarkCheckpointReady(ctx context.Context, tx pgx.Tx, env, checkpoint uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE checkpoints SET status='ready',ready_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, checkpoint); err != nil {
		return err
	}
	// Input admitted during capture already owned the source allocation. Transfer
	// that entitlement to one bounded pending episode as the source becomes optional.
	if _, err := tx.Exec(ctx, `UPDATE computers c SET execution_deadline_at=clock_timestamp()+(SELECT preparation_timeout_ms FROM environments WHERE id=$1)*interval '1 millisecond'
 WHERE c.environment_id=$1 AND c.id=(SELECT computer_id FROM checkpoints WHERE environment_id=$1 AND id=$2) AND c.execution_deadline_at IS NULL
 AND (EXISTS(SELECT 1 FROM turns t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id) WHERE t.environment_id=c.environment_id AND t.computer_id=c.id AND t.status='queued' AND `+db.NoSessionHoldsSQL+`) OR EXISTS(SELECT 1 FROM commands cmd WHERE cmd.environment_id=c.environment_id AND cmd.computer_id=c.id AND cmd.status='pending') OR EXISTS(SELECT 1 FROM computer_controls control WHERE control.environment_id=c.environment_id AND control.computer_id=c.id AND control.kind='wake' AND control.status='accepted'))`, env, checkpoint); err != nil {
		return err
	}
	return settleHibernateCapture(ctx, tx, env, checkpoint, "")
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

// CheckpointDiskEligible is shared by restore admission and logical-process loss.
// A newer or unresolved disk cut makes a retained RAM image ineligible.
func CheckpointDiskEligible(ctx context.Context, tx pgx.Tx, env, computerID, save uuid.UUID) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(c.integrity_fault_at IS NULL AND c.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked WHERE revoked.environment_id=c.environment_id AND revoked.computer_id=c.id) AND c.recovery_save_id=$3
   AND NOT EXISTS(SELECT 1 FROM computer_saves s WHERE s.environment_id=c.environment_id AND s.computer_id=c.id AND s.status IN ('requested','captured')),false)
   FROM computers c WHERE c.environment_id=$1 AND c.id=$2`, env, computerID, save).Scan(&eligible)
	return eligible, err
}
