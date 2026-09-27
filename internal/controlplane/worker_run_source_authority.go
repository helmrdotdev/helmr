package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleWorkerRunSource = errors.New("worker run source authority is stale")

type workerRunSourceAuthority struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	DeploymentID  pgtype.UUID
	ComputerID    pgtype.UUID
	RunID         pgtype.UUID
	AttemptNumber int32
}

func authorizeWorkerRunSource(ctx context.Context, tx pgx.Tx, worker workerActor, lease workerapi.RunLeaseFence) (workerRunSourceAuthority, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: invalid receipt", errStaleWorkerRunSource)
	}
	authority, err := run.LockLiveExecution(ctx, tx, workerExecutionFence(worker, parsed, lease))
	return validateWorkerRunSource(authority, err)
}

func authorizeWorkerRunSourceForComputer(ctx context.Context, tx pgx.Tx, worker workerActor, lease workerapi.RunLeaseFence, target pgtype.UUID) (workerRunSourceAuthority, error) {
	parsed, err := parseRunLeaseFence(lease)
	if err != nil {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: invalid receipt", errStaleWorkerRunSource)
	}
	authority, err := run.LockLiveExecutionForComputer(ctx, tx, workerExecutionFence(worker, parsed, lease), target)
	if errors.Is(err, run.ErrExecutionTargetNotFound) {
		return workerRunSourceAuthority{}, errComputerNotFound
	}
	return validateWorkerRunSource(authority, err)
}

func workerExecutionFence(worker workerActor, parsed parsedRunLeaseFence, lease workerapi.RunLeaseFence) run.ExecutionFence {
	return run.ExecutionFence{LeaseID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID), WorkerEpoch: worker.WorkerEpoch, GroupClaimVersion: worker.GroupClaimVersion, HostClaimVersion: worker.ClaimVersion}
}

func validateWorkerRunSource(authority run.ExecutionAuthority, err error) (workerRunSourceAuthority, error) {
	if errors.Is(err, run.ErrExecutionWorkerClaims) {
		return workerRunSourceAuthority{}, errStaleWorkerClaims
	}
	if err != nil {
		return workerRunSourceAuthority{}, staleWorkerRunSource(err)
	}
	if authority.Run.Status != db.RunStatusRunning || authority.Lease.Status != db.RunLeaseStatusRunning || !authority.Run.ActiveStartedAt.Valid || !authority.Attempt.EntrypointEnteredAt.Valid || authority.Attempt.TerminalAt.Valid || authority.Lease.FinalizationOperationID.Valid {
		return workerRunSourceAuthority{}, fmt.Errorf("%w: live authority mismatch", errStaleWorkerRunSource)
	}
	return workerRunSourceAuthority{OrgID: authority.Run.OrgID, ProjectID: authority.Run.ProjectID, EnvironmentID: authority.Run.EnvironmentID, DeploymentID: authority.Run.DeploymentID, ComputerID: authority.Computer.ID, RunID: authority.Run.ID, AttemptNumber: authority.Attempt.Number}, nil
}

func staleWorkerRunSource(err error) error {
	if err == nil || errors.Is(err, pgx.ErrNoRows) ||
		errors.Is(err, errStaleRunLeaseClaim) {
		return errStaleWorkerRunSource
	}
	return err
}
