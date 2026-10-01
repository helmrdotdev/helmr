package secret

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Revocation is one committed Secret revocation: the Secret, its
// Environment and the revocation generation it reached.
type Revocation struct {
	EnvironmentID uuid.UUID
	SecretID      uuid.UUID
	Generation    int64
}

// Validate reports whether the revocation names its Environment, its Secret
// and a positive generation.
func (r Revocation) Validate() error {
	if r.EnvironmentID == uuid.Nil() || r.SecretID == uuid.Nil() ||
		r.Generation <= 0 {
		return errors.New("secret revocation authority is required")
	}
	return nil
}

// CheckRevocation locks the Computer's complete Secret set in tx and reports
// whether the Computer still places the revoked Secret at the revocation's
// generation. Callers lock their execution rows only after it returns true.
func CheckRevocation(
	ctx context.Context,
	tx pgx.Tx,
	computerID uuid.UUID,
	revocation Revocation,
) (bool, error) {
	rows, err := db.New(tx).LockComputerSecretsForAdmission(
		ctx,
		pgvalue.UUID(computerID),
	)
	if err != nil {
		return false, fmt.Errorf("lock computer secret set for revocation: %w", err)
	}
	if len(rows) > maxComputerSecretPlacements {
		return false, errors.New("computer secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.SecretID == pgvalue.UUID(revocation.SecretID) {
			return row.SecretStatus == "revoked" &&
				row.RevocationGeneration == revocation.Generation, nil
		}
	}
	return false, nil
}
