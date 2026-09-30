package run

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrStaleSource reports that the fenced execution is not a live, entered
// source Run: it is absent, not running, not entered, terminal or
// finalizing.
var ErrStaleSource = errors.New("run source authority is stale")

// LiveSource is the scope of a live source Run that acts on other
// resources, such as creating or deleting a Computer.
type LiveSource struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	DeploymentID  pgtype.UUID
	ComputerID    pgtype.UUID
	RunID         pgtype.UUID
	AttemptNumber int32
}

// LockLiveSource locks the fenced execution as LockLiveExecution does and
// requires a live source. Secret locks, when needed, precede it.
func LockLiveSource(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (LiveSource, error) {
	return CheckLiveSource(LockLiveExecution(ctx, tx, fence))
}

// LockLiveSourceForComputer locks the fenced execution together with the
// target Computer as LockLiveExecutionForComputer does and requires a live
// source. A target outside the source's environment is computer.ErrNotFound.
func LockLiveSourceForComputer(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (LiveSource, error) {
	authority, err := LockLiveExecutionForComputer(ctx, tx, fence, target)
	if errors.Is(err, ErrExecutionTargetNotFound) {
		return LiveSource{}, computer.ErrNotFound
	}
	return CheckLiveSource(authority, err)
}

// CheckLiveSource validates the result of a live execution lock as a live
// source. A missing execution is ErrStaleSource; other lock failures,
// including stale worker claims, are returned unchanged.
func CheckLiveSource(authority ExecutionAuthority, err error) (LiveSource, error) {
	if errors.Is(err, pgx.ErrNoRows) {
		return LiveSource{}, ErrStaleSource
	}
	if err != nil {
		return LiveSource{}, err
	}
	if authority.Run.Status != db.RunStatusRunning || authority.Lease.Status != db.RunLeaseStatusRunning || !authority.Run.ActiveStartedAt.Valid || !authority.Attempt.EntrypointEnteredAt.Valid || authority.Attempt.TerminalAt.Valid || authority.Lease.FinalizationOperationID.Valid {
		return LiveSource{}, fmt.Errorf("%w: live authority mismatch", ErrStaleSource)
	}
	return LiveSource{OrgID: authority.Run.OrgID, ProjectID: authority.Run.ProjectID, EnvironmentID: authority.Run.EnvironmentID, DeploymentID: authority.Run.DeploymentID, ComputerID: authority.Computer.ID, RunID: authority.Run.ID, AttemptNumber: authority.Attempt.Number}, nil
}

func lockLiveSourceTx(ctx context.Context, txb db.TxBeginner, fence ExecutionFence) (LiveSource, error) {
	var source LiveSource
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		source, err = LockLiveSource(ctx, tx, fence)
		return err
	})
	return source, err
}
