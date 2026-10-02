package computer

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (r *Retention) collectCheckpointArtifacts(ctx context.Context) error {
	if _, err := r.queries.ReleaseInvalidCheckpointArtifacts(ctx, 100); err != nil {
		return err
	}
	candidates, err := r.queries.ListUnreferencedCheckpointArtifacts(ctx, 100)
	if err != nil {
		return err
	}
	var failures []error
	for _, candidate := range candidates {
		err := r.collectCheckpointArtifact(ctx, candidate)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" {
			continue
		}
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (r *Retention) collectCheckpointArtifact(ctx context.Context, candidate db.ListUnreferencedCheckpointArtifactsRow) error {
	return db.RunTx(ctx, r.pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		n, err := q.DeleteUnreferencedCheckpointArtifact(ctx, candidate.ID)
		if err != nil || n == 0 {
			return err
		}
		// Serialize shared membership cleanup after logical-owner deletion.
		if _, err = q.LockCollectedComputerBlob(ctx, candidate.Digest); err != nil {
			return err
		}
		if _, err = q.DeleteUnreferencedComputerCasMembership(ctx, db.DeleteUnreferencedComputerCasMembershipParams{OrgID: candidate.OrgID, Digest: candidate.Digest}); err != nil {
			return err
		}
		_, err = q.RetireCollectedComputerObject(ctx, candidate.Digest)
		return err
	})
}
