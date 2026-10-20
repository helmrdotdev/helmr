package computer

import (
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootseed"
	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// SelectBootSeed pins an optional exact-content optimization for the initial
// admitted cohort. Missing or incompatible artifacts leave ordinary boot
// available. This never selects an image, changes a root or authorizes a Program.
func SelectBootSeed(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity LeaseIdentity, deployment uuid.UUID, program computercheckpoint.Program) (*bootseed.Manifest, error) {
	if !identity.Valid() || deployment == uuid.Nil() || program.Validate() != nil {
		return nil, ErrInvalidInput
	}
	var result *bootseed.Manifest
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := shortLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := LockHost(ctx, tx, host); err != nil {
			return err
		}
		var spec uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT image_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, identity.EnvironmentID, identity.ComputerID).Scan(&spec); err != nil {
			return err
		}
		if err := LockImageSpec(ctx, tx, identity.EnvironmentID, spec); err != nil {
			return err
		}
		var image uuid.UUID
		var root string
		if err := tx.QueryRow(ctx, `SELECT image_id,'sha256:'||encode(initial_root_digest,'hex') FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID).Scan(&image, &root); err != nil {
			return err
		}
		if err := checkComputerDiskAuthority(ctx, tx, host, identity); err != nil {
			return err
		}
		var selected *uuid.UUID
		var shape AllocationShape
		var fresh bool
		if err := tx.QueryRow(ctx, `SELECT boot_seed_build_id,reserved_cpu_millis,reserved_memory_bytes,reserved_scratch_bytes,vm_platform_id,vm_vcpu_count,cpu_config_digest,
 status='acquiring' AND initialized_at IS NULL AND restored_from_save_id IS NULL AND NOT disk_only_recovery
 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).
			Scan(&selected, &shape.CPUMillis, &shape.MemoryBytes, &shape.ScratchBytes, &shape.VMPlatformID, &shape.VCPUCount, &shape.CPUConfigDigest, &fresh); err != nil {
			return err
		}
		if !fresh {
			return nil
		}
		var cohort *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT (SELECT s.deployment_id FROM session_processes p JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.environment_id=$1 AND p.computer_id=$2 AND p.computer_lease_epoch=$3 ORDER BY p.session_id,p.epoch LIMIT 1)`,
			identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&cohort); err != nil {
			return err
		}
		if cohort == nil || *cohort != deployment {
			return ErrConflict
		}
		var id uuid.UUID
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT b.id,b.manifest FROM boot_seed_builds b
 WHERE b.environment_id=$1 AND b.deployment_id=$2 AND b.image_id=$3 AND b.status='succeeded' AND b.payload_retired_at IS NULL
 AND b.vm_platform_id=$4 AND b.vm_vcpu_count=$5 AND b.cpu_config_digest=$6 AND b.reserved_memory_bytes=$7 AND b.reserved_scratch_bytes=$8
 AND ($9::uuid IS NULL OR b.id=$9) AND NOT EXISTS(SELECT 1 FROM computer_leases l WHERE l.environment_id=b.environment_id AND l.computer_id=$10 AND l.boot_seed_build_id=b.id AND l.boot_seed_failed_at IS NOT NULL)
 ORDER BY b.id LIMIT 1 FOR KEY SHARE`, identity.EnvironmentID, deployment, image, shape.VMPlatformID, shape.VCPUCount, shape.CPUConfigDigest, shape.MemoryBytes, shape.ScratchBytes, selected, identity.ComputerID).Scan(&id, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var m bootseed.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		if err := m.Validate(); err != nil {
			return err
		}
		content, err := m.Content()
		if err != nil {
			return err
		}
		if m.EnvironmentID != identity.EnvironmentID || m.BuildID != id || content.ComputerRoot != root ||
			content.Program == nil || *content.Program != program {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE computer_leases SET boot_seed_build_id=$4 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, id); err != nil {
			return err
		}
		result = &m
		return checkComputerDiskAuthority(ctx, tx, host, identity)
	})
	return result, notReadyOnLock(err)
}

// RecordBootSeedFailure excludes this exact artifact only for this Computer.
// A worker report does not revoke shared publication eligibility.
func RecordBootSeedFailure(ctx context.Context, tx pgx.Tx, identity LeaseIdentity, build uuid.UUID) error {
	if build == uuid.Nil() {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE computer_leases SET boot_seed_failed_at=COALESCE(boot_seed_failed_at,clock_timestamp())
 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND allocation_id=$4 AND boot_seed_build_id=$5`,
		identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, build)
	if err != nil {
		return err
	}
	// The optional artifact hint never blocks exact physical-stop evidence.
	// A mismatch excludes nothing.
	return nil
}
