package computer

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Admission is a Computer locked, with its Environment's scope and committed
// head disk version, for admitting a new logical member in the owning
// transaction. The caller locks the Secrets the member resolves first, decides
// from Row whether the Computer admits the member, and records the admission
// with Touch. An Admission is valid only inside the transaction that locked
// it.
type Admission struct {
	tx  pgx.Tx
	row db.LockComputerAdmissionAuthorityRow
}

// LockForAdmission update-locks the Computer for admitting a new member. A
// Computer that does not exist in the Environment, or has no initializing or
// committed head disk version, returns ErrNotFound.
func LockForAdmission(ctx context.Context, tx pgx.Tx, environmentID, computerID uuid.UUID) (Admission, error) {
	row, err := db.New(tx).LockComputerAdmissionAuthority(ctx, db.LockComputerAdmissionAuthorityParams{
		EnvironmentID: pgvalue.UUID(environmentID), ID: pgvalue.UUID(computerID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Admission{}, ErrNotFound
	}
	if err != nil {
		return Admission{}, fmt.Errorf("lock computer admission authority: %w", err)
	}
	return Admission{tx: tx, row: row}, nil
}

// Row is the locked Computer with its organization and project.
func (a Admission) Row() db.LockComputerAdmissionAuthorityRow {
	return a.row
}

// LockLiveInstance update-locks the admitted Computer's unreclaimed Instance
// when it has one, so the caller's member operation holds the Computer and
// its Instance in the global order before it locks member rows. A Computer
// without an unreclaimed Instance is not an error.
//
// Equivalence: this is one statement, the unreclaimed Instance of the locked
// Computer FOR UPDATE, addressed by the locked row's Environment and id,
// with no admission, writer or readiness predicate; a missing Instance is
// skipped and any other error is returned unchanged.
func (a Admission) LockLiveInstance(ctx context.Context) error {
	if a.tx == nil {
		return errors.New("computer admission is not locked")
	}
	_, err := db.New(a.tx).LockComputerInstance(ctx, db.LockComputerInstanceParams{
		EnvironmentID: a.row.EnvironmentID, ComputerID: a.row.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

// Touch records the admission: the Computer becomes desired active and its
// revision and activity advance. It fails with pgx.ErrNoRows unless the
// locked Computer is still active, clean, free of preparation and recovery
// failures, and at the locked revision.
func (a Admission) Touch(ctx context.Context) error {
	if a.tx == nil {
		return errors.New("computer admission is not locked")
	}
	_, err := db.New(a.tx).TouchComputerForAdmission(ctx, db.TouchComputerForAdmissionParams{
		EnvironmentID: a.row.EnvironmentID, ID: a.row.ID, ExpectedRevision: a.row.Revision,
	})
	return err
}
