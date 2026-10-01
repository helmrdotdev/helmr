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

// Residence fences lock a Computer and then its unreclaimed Instance, when
// it has one, for a reconciler that settles a member's Wait in the caller's
// transaction. They check no Computer or Instance state: the caller decides
// from its member rows, which it locks after the fence, whether the Wait
// still settles. A Computer outside the addressed scope returns ErrNotFound
// before any Instance statement.

// LockResidence update-locks the Computer in its Environment, then its
// unreclaimed Instance when it has one. The Computer's Secrets, when the
// caller locks them, come first; the caller locks the Session, Run, Attempt
// and Wait after.
//
// Equivalence: the Computer statement is the plain Environment and id
// Computer lock FOR UPDATE with no status predicate, and the Instance
// statement is the unreclaimed Instance of that Computer FOR UPDATE; a
// missing Instance is skipped.
func LockResidence(ctx context.Context, tx pgx.Tx, environmentID, computerID uuid.UUID) error {
	q := db.New(tx)
	environment, id := pgvalue.UUID(environmentID), pgvalue.UUID(computerID)
	_, err := q.LockTokenWaitComputer(ctx, db.LockTokenWaitComputerParams{ComputerID: id, EnvironmentID: environment})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("lock computer: %w", err)
	}
	_, err = q.LockComputerInstance(ctx, db.LockComputerInstanceParams{ComputerID: id, EnvironmentID: environment})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("lock computer instance: %w", err)
	}
	return nil
}

// RunResidenceRef addresses the Computer of a Run in the Run's organization,
// project, Environment and region.
type RunResidenceRef struct {
	OrgID         uuid.UUID
	ProjectID     uuid.UUID
	EnvironmentID uuid.UUID
	RegionID      string
	ComputerID    uuid.UUID
}

// RunResidence is a Run's Computer locked, with its unreclaimed Instance
// when it has one, by LockRunResidence. It is valid only inside the
// transaction that locked it.
type RunResidence struct {
	computer db.LockRunLeaseClaimComputerRow
}

// LockRunResidence update-locks the Computer in the Run's scope, then, only
// when the scope matches, its unreclaimed Instance when it has one. A
// Computer outside the scope returns ErrNotFound and locks no Instance.
//
// Equivalence: the Computer statement is the Run lease scoped Computer lock
// (organization, project, Environment and region) FOR UPDATE, and the
// Instance statement, issued only after that statement returned a row, is
// the unreclaimed Instance of the Computer FOR UPDATE; a missing Instance is
// skipped and other errors are returned unchanged.
func LockRunResidence(ctx context.Context, tx pgx.Tx, ref RunResidenceRef) (RunResidence, error) {
	q := db.New(tx)
	environment, id := pgvalue.UUID(ref.EnvironmentID), pgvalue.UUID(ref.ComputerID)
	c, err := q.LockRunLeaseClaimComputer(ctx, db.LockRunLeaseClaimComputerParams{
		ID: id, OrgID: pgvalue.UUID(ref.OrgID), ProjectID: pgvalue.UUID(ref.ProjectID),
		EnvironmentID: environment, RegionID: ref.RegionID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return RunResidence{}, ErrNotFound
	}
	if err != nil {
		return RunResidence{}, err
	}
	if _, err = q.LockComputerInstance(ctx, db.LockComputerInstanceParams{EnvironmentID: environment, ComputerID: id}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return RunResidence{}, err
	}
	return RunResidence{computer: c}, nil
}

// Computer is the locked Computer.
func (r RunResidence) Computer() db.LockRunLeaseClaimComputerRow {
	return r.computer
}
