package artifactgc

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5/pgconn"
)

// Only unrooted graph objects are eligible. Root lifetime belongs to its head,
// checkpoint, attempt and Runtime owners; this sweep never expires that history.
func (r *Reclaimer) collectComputerObjects(ctx context.Context) error {
	candidates, err := r.queries.ListUnreferencedComputerObjects(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		if err = r.collectComputerObject(ctx, candidate); err != nil {
			var pe *pgconn.PgError
			if errors.As(err, &pe) && pe.Code == "23503" {
				continue
			} // Concurrent adoption won.
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Reclaimer) collectComputerObject(ctx context.Context, candidate db.ListUnreferencedComputerObjectsRow) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	q := db.New(tx)
	n, err := q.DeleteUnreferencedComputerObject(ctx, db.DeleteUnreferencedComputerObjectParams{EnvironmentID: candidate.EnvironmentID, ComputerID: candidate.ComputerID, Digest: candidate.Digest})
	if err != nil || n == 0 {
		return err
	}
	if _, err = q.LockCollectedComputerLifetime(ctx, candidate.Digest); err != nil {
		return err
	}
	if _, err = q.DeleteUnreferencedComputerCasMembership(ctx, db.DeleteUnreferencedComputerCasMembershipParams{OrgID: candidate.OrgID, Digest: candidate.Digest}); err != nil {
		return err
	}
	if _, err = q.RetireCollectedComputerObject(ctx, candidate.Digest); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
