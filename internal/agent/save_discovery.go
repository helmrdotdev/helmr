package agent

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// NextComputerSave returns the oldest unresolved request for this exact writer.
// Checkpoint cuts remain owned by coherent capture; an earlier unresolved one
// also blocks a later live cut. A due owner may admit one optional disk save
// only after rechecking pending work under the Computer lock.
func NextComputerSave(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity, backgroundDue bool) (*SaveRequest, error) {
	if !identity.valid() {
		return nil, ErrInvalidInput
	}
	var result *SaveRequest
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND computer_instance_id=$4 AND worker_host_id=$5 AND worker_epoch=$6 AND status IN ('active','releasing') AND fenced_at IS NULL AND expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return ErrDenied
		}
		pending, checkpoint, err := oldestComputerSave(ctx, tx, identity)
		if err != nil || pending != nil || checkpoint || !backgroundDue {
			result = pending
			return err
		}
		// Normal discovery remains read-only. Optional admission serializes with
		// finalization, checkpoint admission and physical lease transitions.
		var computer uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, identity.EnvironmentID, identity.ComputerID).Scan(&computer); err != nil {
			return err
		}
		pending, checkpoint, err = oldestComputerSave(ctx, tx, identity)
		if err != nil || pending != nil || checkpoint {
			result = pending
			return err
		}
		if err := computerDispatchAvailable(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			if errors.Is(err, ErrNotReady) {
				return nil
			}
			return err
		}
		// Group/Host locks exclude retirement, including Pool retirement which
		// takes the Group lock. Draining owners may finish existing requests but
		// must not create new optional work.
		var eligible bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM computer_leases l
 JOIN worker_hosts h ON h.id=l.worker_host_id AND h.status='active'
 JOIN worker_groups g ON g.id=h.worker_group_id AND g.status='active'
 JOIN worker_pools p ON p.id=h.worker_pool_id AND p.worker_group_id=g.id AND p.status='active'
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.computer_instance_id=$4
 AND l.worker_host_id=$5 AND l.worker_epoch=$6 AND l.status='active'
 AND l.fenced_at IS NULL AND l.expires_at>clock_timestamp())`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&eligible); err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		save := SaveRequest{ID: uuid.NewV7(), ComputerID: identity.ComputerID, LeaseEpoch: identity.Epoch}
		err = tx.QueryRow(ctx, `UPDATE computers SET next_save_seq=next_save_seq+1 WHERE environment_id=$1 AND id=$2 AND integrity_fault_at IS NULL AND deleted_at IS NULL RETURNING next_save_seq-1`, identity.EnvironmentID, identity.ComputerID).Scan(&save.Sequence)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq) VALUES($1,$2,$3,$4,$5) RETURNING requested_at`, identity.EnvironmentID, save.ID, save.ComputerID, save.LeaseEpoch, save.Sequence).Scan(&save.RequestedAt); err != nil {
			return err
		}
		result = &save
		return nil
	})
	return result, allocationError(err)
}

// The oldest request controls ordering even when a checkpoint owns its cut.
func oldestComputerSave(ctx context.Context, tx pgx.Tx, identity ComputerLeaseIdentity) (*SaveRequest, bool, error) {
	var save SaveRequest
	var checkpoint bool
	err := tx.QueryRow(ctx, `SELECT s.id,s.computer_id,s.computer_lease_epoch,s.seq,s.requested_at,
 EXISTS(SELECT 1 FROM computer_checkpoints p WHERE p.environment_id=s.environment_id AND p.disk_save_id=s.id)
 FROM computer_saves s WHERE s.environment_id=$1 AND s.computer_id=$2 AND s.computer_lease_epoch=$3
 AND s.status IN ('requested','captured') ORDER BY s.seq LIMIT 1`, identity.EnvironmentID, identity.ComputerID, identity.Epoch).Scan(&save.ID, &save.ComputerID, &save.LeaseEpoch, &save.Sequence, &save.RequestedAt, &checkpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil || checkpoint {
		return nil, checkpoint, err
	}
	return &save, false, nil
}
