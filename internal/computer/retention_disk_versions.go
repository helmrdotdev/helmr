package computer

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// Retiring payload never removes immutable Version lineage or receipts. Native
// owner FKs, rather than the discovery snapshot, decide whether it may disappear.
func (r *Retention) collectComputerDiskVersions(ctx context.Context) error {
	candidates, err := r.queries.ListUnreferencedComputerDiskVersionRoots(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err = r.collectComputerDiskVersion(ctx, candidate); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && (pg.Code == "23503" || pg.Code == "40P01") {
				continue
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Retention) collectComputerDiskVersion(ctx context.Context, c db.ListUnreferencedComputerDiskVersionRootsRow) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	n, err := q.DeleteUnreferencedComputerDiskVersionRoot(ctx, db.DeleteUnreferencedComputerDiskVersionRootParams(c))
	if err != nil || n == 0 {
		return err
	}
	n, err = q.RetireComputerDiskVersionPayload(ctx, db.RetireComputerDiskVersionPayloadParams(c))
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("computer payload retirement lost Version identity")
	}
	return tx.Commit(ctx)
}
