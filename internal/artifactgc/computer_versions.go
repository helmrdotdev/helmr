package artifactgc

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// Retiring payload never removes immutable Version lineage or receipts. Native
// owner FKs, rather than the discovery snapshot, decide whether it may disappear.
func (r *Reclaimer) collectComputerVersions(ctx context.Context) error {
	candidates, err := r.queries.ListUnreferencedComputerVersionRoots(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err = r.collectComputerVersion(ctx, candidate); err != nil {
			var pg *pgconn.PgError
			if errors.As(err, &pg) && (pg.Code == "23503" || pg.Code == "40P01") {
				continue
			}
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Reclaimer) collectComputerVersion(ctx context.Context, c db.ListUnreferencedComputerVersionRootsRow) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	n, err := q.DeleteUnreferencedComputerVersionRoot(ctx, db.DeleteUnreferencedComputerVersionRootParams{EnvironmentID: c.EnvironmentID, ComputerID: c.ComputerID, VersionID: c.VersionID})
	if err != nil || n == 0 {
		return err
	}
	n, err = q.RetireComputerVersionPayload(ctx, db.RetireComputerVersionPayloadParams{EnvironmentID: c.EnvironmentID, ComputerID: c.ComputerID, VersionID: c.VersionID})
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("Computer payload retirement lost Version identity")
	}
	return tx.Commit(ctx)
}
