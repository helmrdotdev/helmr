package computer

import (
	"context"
	"github.com/jackc/pgx/v5"
	"time"
	"uuid"
)

// RecordLeaseStoragePressure is called under the Computer lock after writer
// authority validation, or by irrevocable lease-loss settlement.
func RecordLeaseStoragePressure(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID, epoch int64, observedSince *time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE computer_leases SET storage_pressure_since=CASE WHEN $4::timestamptz IS NULL THEN NULL ELSE COALESCE(storage_pressure_since,clock_timestamp()) END,storage_pressure_observed_since=$4 WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, computer, epoch, observedSince)
	return err
}
