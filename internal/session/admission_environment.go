package session

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/org"
	"github.com/jackc/pgx/v5"
)

func computerLimits(p org.AdmissionPolicy) computer.Limits {
	return computer.Limits{MaxCPU: p.MaxCPU, MaxMemory: p.MaxMemory, MaxStorage: p.MaxStorage, PreparationTimeout: p.PreparationTimeout}
}

// Creation (with or without input) and later Turn admission share one token
// bucket; historical receipts consume none. The outstanding-Turn limit counts
// Turns, which this owner keeps.
func consumeAdmission(ctx context.Context, tx pgx.Tx, env uuid.UUID, p org.AdmissionPolicy) error {
	var outstanding int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM turns WHERE environment_id=$1 AND status IN ('queued','running')`, env).Scan(&outstanding); err != nil {
		return err
	}
	if outstanding >= p.MaxOutstanding {
		return ErrNotReady
	}
	return consumeAdmissionToken(ctx, tx, env)
}

func consumeAdmissionToken(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	charged, err := org.ChargeAdmission(ctx, tx, env)
	if err != nil {
		return err
	}
	if !charged {
		return ErrNotReady
	}
	return nil
}
