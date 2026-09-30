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
// resources, such as creating or deleting a Computer. Only a locked Execution
// yields one.
type LiveSource struct {
	orgID         pgtype.UUID
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	computerID    pgtype.UUID
	runID         pgtype.UUID
}

// OrgID is the source Run's organization.
func (s LiveSource) OrgID() pgtype.UUID { return s.orgID }

// ProjectID is the source Run's project.
func (s LiveSource) ProjectID() pgtype.UUID { return s.projectID }

// EnvironmentID is the source Run's environment.
func (s LiveSource) EnvironmentID() pgtype.UUID { return s.environmentID }

// ComputerID is the source Run's Computer.
func (s LiveSource) ComputerID() pgtype.UUID { return s.computerID }

// RunID is the source Run.
func (s LiveSource) RunID() pgtype.UUID { return s.runID }

// LockLiveSource locks the fenced execution as LockLiveExecution does and
// requires a live source. Secret locks, when needed, precede it.
func LockLiveSource(ctx context.Context, tx pgx.Tx, fence ExecutionFence) (LiveSource, error) {
	_, source, err := checkLiveSource(LockLiveExecution(ctx, tx, fence))
	return source, err
}

// LockLiveSourceForComputer locks the fenced execution together with the
// target Computer as LockLiveExecutionForComputer does and requires a live
// source. A target outside the source's environment is computer.ErrNotFound.
func LockLiveSourceForComputer(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (LiveSource, error) {
	execution, err := LockLiveExecutionForComputer(ctx, tx, fence, target)
	if errors.Is(err, ErrExecutionTargetNotFound) {
		return LiveSource{}, computer.ErrNotFound
	}
	_, source, err := checkLiveSource(execution, err)
	return source, err
}

// LiveSource requires the locked execution to be a live source: its Run and
// lease are running, its attempt has entered and is not terminal, and its
// lease is not finalizing. Otherwise it returns ErrStaleSource.
func (e Execution) LiveSource() (LiveSource, error) {
	if e.run.Status != db.RunStatusRunning || e.lease.Status != db.RunLeaseStatusRunning || !e.run.ActiveStartedAt.Valid || !e.attempt.EntrypointEnteredAt.Valid || e.attempt.TerminalAt.Valid || e.lease.FinalizationOperationID.Valid {
		return LiveSource{}, fmt.Errorf("%w: live authority mismatch", ErrStaleSource)
	}
	return LiveSource{orgID: e.run.OrgID, projectID: e.run.ProjectID, environmentID: e.run.EnvironmentID, computerID: e.Computer().ID, runID: e.run.ID}, nil
}

// checkLiveSource validates the result of a live execution lock as a live
// source. A missing execution is ErrStaleSource; other lock failures,
// including stale worker claims, are returned unchanged.
func checkLiveSource(execution Execution, err error) (Execution, LiveSource, error) {
	if err != nil {
		return Execution{}, LiveSource{}, staleSource(err)
	}
	source, err := execution.LiveSource()
	if err != nil {
		return Execution{}, LiveSource{}, err
	}
	return execution, source, nil
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
