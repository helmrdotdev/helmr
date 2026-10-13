package session

import (
	"context"
	"encoding/json"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"

	"github.com/helmrdotdev/helmr/internal/computercheckpoint"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ReadComputerCheckpoint supplies only the coherent image bound to an admitted,
// delivered restore allocation. It grants neither installation nor activation.
func ReadComputerCheckpoint(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, identity computer.LeaseIdentity) (computercheckpoint.Manifest, error) {
	if !identity.Valid() {
		return computercheckpoint.Manifest{}, ErrInvalidInput
	}
	var result computercheckpoint.Manifest
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := computer.LockHost(ctx, tx, host); err != nil {
			return err
		}
		var checkpoint uuid.UUID
		err := tx.QueryRow(ctx, `SELECT cp.id FROM computer_leases l JOIN checkpoints cp
 ON (cp.environment_id,cp.computer_id,cp.disk_save_id)=(l.environment_id,l.computer_id,l.restored_from_save_id)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3 AND l.allocation_id=$4
 AND l.worker_host_id=$5 AND l.worker_epoch=$6 AND l.delivered_at IS NOT NULL
 AND cp.status IN ('ready','restoring')`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&checkpoint)
		if err != nil {
			return err
		}
		r, err := LockCapture(ctx, tx, identity.EnvironmentID, checkpoint)
		if err != nil {
			return err
		}
		if r.State != "ready" && r.State != "restoring" {
			return ErrNotReady
		}
		target, err := restoreTargetLease(ctx, tx, host, identity.EnvironmentID, r, identity.Epoch)
		if err != nil {
			return err
		}
		if target.Instance != identity.InstanceID {
			return ErrDenied
		}
		if err := computer.RequireSecretsNotRevoked(ctx, tx, identity.EnvironmentID, identity.ComputerID); err != nil {
			return err
		}
		eligible, err := computer.CheckpointDiskEligible(ctx, tx, identity.EnvironmentID, identity.ComputerID, r.Save)
		if err != nil {
			return err
		}
		if !eligible {
			return ErrNotReady
		}
		var fenced bool
		if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, identity.EnvironmentID, identity.ComputerID, r.SourceEpoch).Scan(&fenced); err != nil {
			return err
		}
		if !fenced {
			return ErrNotReady
		}
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT manifest FROM checkpoints WHERE environment_id=$1 AND id=$2`, identity.EnvironmentID, checkpoint).Scan(&raw); err != nil {
			return err
		}
		if json.Unmarshal(raw, &result) != nil {
			return ErrNotReady
		}
		if _, err := result.Encode(); err != nil {
			return ErrNotReady
		}
		return computer.ValidateCheckpointRuntime(ctx, tx, host, result.Runtime)
	})
	if err != nil {
		return computercheckpoint.Manifest{}, hideMissing(err)
	}
	return result, nil
}
