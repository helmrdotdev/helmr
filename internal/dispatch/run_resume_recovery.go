package dispatch

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
)

// RecoverExpiredRunResumes repairs suspended runs whose resume lease expired.
// Every resume restores its checkpoint on ordinary compatible capacity.
func (d *Authority) RecoverExpiredRunResumes(
	ctx context.Context,
	limit int32,
) ([]db.RecoverExpiredRunResumesRow, error) {
	if limit <= 0 {
		return nil, nil
	}
	return db.New(d.pool).RecoverExpiredRunResumes(ctx, limit)
}
