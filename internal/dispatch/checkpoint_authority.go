package dispatch

import (
	"context"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
)

func (d *Authority) RequestCheckpoint(ctx context.Context, params db.ClaimRunCheckpointWaitParams) (db.ClaimRunCheckpointWaitRow, error) {
	tx, err := d.begin(ctx)
	if err != nil {
		return db.ClaimRunCheckpointWaitRow{}, fmt.Errorf("begin checkpoint request: %w", err)
	}
	defer rollback(ctx, tx)
	row, err := db.New(tx).ClaimRunCheckpointWait(ctx, params)
	if err != nil {
		return db.ClaimRunCheckpointWaitRow{}, fmt.Errorf("request checkpoint: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return db.ClaimRunCheckpointWaitRow{}, fmt.Errorf("commit checkpoint request: %w", err)
	}
	return row, nil
}
