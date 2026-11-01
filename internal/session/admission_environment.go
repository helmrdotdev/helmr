package session

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5"
)

// Creation (with or without input) and later Turn admission share one token
// bucket; historical receipts consume none. The outstanding-Turn limit counts
// Turns, which this owner keeps.
func consumeAdmission(ctx context.Context, tx pgx.Tx, limits org.ExecutionLimits, env uuid.UUID) error {
	var outstanding int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running')`, env).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding >= limits.For(env).MaxOutstandingAdmissions {
		return ErrNotReady
	}
	return consumeAdmissionToken(ctx, tx, limits, env)
}

func consumeAdmissionToken(ctx context.Context, tx pgx.Tx, limits org.ExecutionLimits, env uuid.UUID) error {
	charged, err := org.ChargeAdmission(ctx, tx, limits, env)
	if err != nil {
		return err
	}
	if !charged {
		return ErrNotReady
	}
	return nil
}
