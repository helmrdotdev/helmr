package agent

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

// ProcessIdentity is discovery evidence. Attachment and startup independently
// authorize the current process; listing never grants permission to run setup.
type ProcessIdentity struct {
	SessionID uuid.UUID
	Epoch     int64
}

// ListComputerProcesses includes every unfenced process bound to this exact
// physical allocation, including held or failed processes requiring control.
// Expiry or revocation must not hide retained work from its physical owner.
// A deleted Computer requests physical closure through the same stop response;
// it remains charged until the worker acknowledges actual absence.
// An empty page ends the scan; the next poll starts without a cursor.
func ListComputerProcesses(ctx context.Context, database db.TxBeginner, host workergroup.HostPrincipal, identity ComputerLeaseIdentity, after *ProcessIdentity) ([]ProcessIdentity, error) {
	if !identity.valid() || after != nil && (after.SessionID == uuid.Nil() || after.Epoch <= 0) {
		return nil, ErrInvalidInput
	}
	cursor := ProcessIdentity{}
	if after != nil {
		cursor = *after
	}
	var result []ProcessIdentity
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := allocationLockTimeout(ctx, tx); err != nil {
			return err
		}
		if err := lockComputerHost(ctx, tx, host); err != nil {
			return err
		}
		var owned, closed bool
		if err := tx.QueryRow(ctx, `SELECT l.worker_host_id=$5 AND l.worker_epoch=$6 AND l.computer_instance_id=$4,l.fenced_at IS NOT NULL OR c.deleted_at IS NOT NULL
 FROM computer_leases l JOIN computers c ON (c.environment_id,c.id)=(l.environment_id,l.computer_id)
 WHERE l.environment_id=$1 AND l.computer_id=$2 AND l.epoch=$3`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, identity.InstanceID, host.HostID, host.Epoch).Scan(&owned, &closed); err != nil {
			return err
		}
		if !owned {
			return ErrDenied
		}
		if closed {
			return ErrAllocationClosed
		}
		rows, err := tx.Query(ctx, `SELECT session_id,epoch FROM session_processes
 WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch=$3 AND fenced_at IS NULL
 AND (session_id,epoch)>($4,$5) ORDER BY session_id,epoch LIMIT 100`, identity.EnvironmentID, identity.ComputerID, identity.Epoch, cursor.SessionID, cursor.Epoch)
		if err != nil {
			return err
		}
		result, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ProcessIdentity, error) {
			var p ProcessIdentity
			err := row.Scan(&p.SessionID, &p.Epoch)
			return p, err
		})
		return err
	})
	if err != nil {
		return nil, allocationError(err)
	}
	return result, nil
}
