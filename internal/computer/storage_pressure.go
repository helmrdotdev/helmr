package computer

import (
	"context"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// RecordLeaseStoragePressure is called under the Computer lock after writer
// authority validation, or by irrevocable lease-loss settlement.
func RecordLeaseStoragePressure(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64, pressured bool) error {
	changed, err := tx.Exec(ctx, `UPDATE computer_leases SET storage_pressure_since=CASE WHEN $4 THEN clock_timestamp() ELSE NULL END WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3 AND (storage_pressure_since IS NOT NULL) IS DISTINCT FROM $4`, env, computer, epoch, pressured)
	if err != nil || pressured || changed.RowsAffected() == 0 {
		return err
	}
	return notifyComputerChanged(ctx, tx, env, computer)
}
