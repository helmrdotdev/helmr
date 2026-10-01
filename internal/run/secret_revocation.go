package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

// FailSecretRevokedRuns fails up to limit live Runs whose current attempt
// resolved the revoked Secret at an older generation, shallowest first. Each
// Run is handled in its own transaction: it locks the Run's Computer's
// Secrets, and when that Computer still places the Secret at the revoked
// generation, the Run's owned finalization graph, and fails the Run. It
// returns the number of candidates examined, including those it left
// unchanged; on the first failure it returns the candidates examined before
// it with the error.
func FailSecretRevokedRuns(
	ctx context.Context,
	txdb db.TxDB,
	revocation secret.Revocation,
	limit int32,
) (int, error) {
	if err := revocation.Validate(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("secret revocation batch limit must be positive")
	}
	candidates, err := db.New(txdb).ListSecretRevocationRuns(
		ctx,
		db.ListSecretRevocationRunsParams{
			SecretID:             pgvalue.UUID(revocation.SecretID),
			RevocationGeneration: revocation.Generation,
			EnvironmentID:        pgvalue.UUID(revocation.EnvironmentID),
			RowLimit:             limit,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("list secret-revoked run candidates: %w", err)
	}
	examined := 0
	for _, candidate := range candidates {
		if err := db.RunTx(ctx, txdb, func(tx pgx.Tx) error {
			return failSecretRevokedRun(ctx, tx, candidate, revocation)
		}); err != nil {
			return examined, err
		}
		examined++
	}
	return examined, nil
}

func failSecretRevokedRun(
	ctx context.Context,
	tx pgx.Tx,
	candidate db.ListSecretRevocationRunsRow,
	revocation secret.Revocation,
) error {
	revoked, err := secret.CheckRevocation(
		ctx,
		tx,
		pgvalue.MustUUIDValue(candidate.ComputerID),
		revocation,
	)
	if err != nil || !revoked {
		return err
	}
	graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{
		OrgID:         pgvalue.MustUUIDValue(candidate.OrgID),
		ProjectID:     pgvalue.MustUUIDValue(candidate.ProjectID),
		EnvironmentID: pgvalue.MustUUIDValue(candidate.EnvironmentID),
		RunID:         pgvalue.MustUUIDValue(candidate.ID),
	})
	if err == nil {
		_, err = graph.FailCurrentForSecretRevocation(ctx)
	}
	if err != nil {
		return fmt.Errorf("fail secret-revoked run graph: %w", err)
	}
	return nil
}
