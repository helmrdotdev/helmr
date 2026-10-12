package computer

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
)

func ReleaseLeaseDisk(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2 FOR NO KEY UPDATE`, env, computer).Scan(&id); err != nil {
		return err
	}
	var fenced, bound, released bool
	if err := tx.QueryRow(ctx, `SELECT fenced_at IS NOT NULL,base_root_id IS NOT NULL,disk_released_at IS NOT NULL FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 FOR NO KEY UPDATE`, env, computer, epoch).Scan(&fenced, &bound, &released); err != nil {
		return err
	}
	if released || !bound {
		return nil
	}
	if !fenced {
		return ErrNotReady
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computer_saves WHERE environment_id=$1 AND computer_id=$2 AND computer_lease_epoch=$3 AND status IN ('requested','captured'))
 OR EXISTS(SELECT 1 FROM checkpoints WHERE environment_id=$1 AND computer_id=$2 AND (source_lease_epoch=$3 OR target_lease_epoch=$3) AND status NOT IN ('cancelled','lost') AND controls_reconciled_at IS NULL)`, env, computer, epoch).Scan(&pending); err != nil {
		return err
	}
	if pending {
		return ErrNotReady
	}
	_, err := tx.Exec(ctx, `UPDATE computer_leases SET disk_released_at=clock_timestamp() WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch)
	return err
}
