package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleTaskCompletion = errors.New("task completion receipt is stale")

type taskCompletionReplayStore interface {
	GetTaskCompletionReplay(context.Context, db.GetTaskCompletionReplayParams) (pgtype.Text, error)
}

func (s *Server) completeTask(ctx context.Context, worker workergroup.HostPrincipal, request workerapi.CompleteTaskRequest, completion parsedTaskCompletion) error {
	err := s.inTx(ctx, func(work *txWork) error {
		return run.CompleteTaskExecution(ctx, work.tx, run.TaskCompletion{Fence: workerExecutionFence(worker, completion.lease, request.Lease), OperationID: pgvalue.UUID(completion.operationID), Fingerprint: completion.fingerprint, Kind: string(completion.kind), Output: completion.output, Error: completion.errorObject})
	})
	// Stale claims re-authenticate before any replay lookup.
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	// Includes an uncertain transaction commit or a concurrent completion that
	// committed before this request could acquire live locators.
	if err != nil {
		err = taskCompletionReplayAfterError(ctx, s.db, worker, request, completion, err)
	}
	if errors.Is(err, run.ErrTaskCompletionAdmission) {
		return deterministicWorkerAdmission(err)
	}

	if err == nil {
		return nil
	}
	return staleAuthority(staleAuthorityTaskCompletion, "execution", staleTaskCompletion(err))
}

func taskCompletionWasReplayed(
	ctx context.Context,
	store taskCompletionReplayStore,
	worker workergroup.HostPrincipal,
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
) (bool, error) {
	fingerprint, err := store.GetTaskCompletionReplay(ctx, db.GetTaskCompletionReplayParams{
		RunLeaseID:    pgvalue.UUID(completion.lease.leaseID),
		LeaseSequence: request.Lease.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.GroupID),
		WorkerHostID: pgvalue.UUID(worker.HostID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !fingerprint.Valid || fingerprint.String != completion.fingerprint {
		return false, staleAuthority(staleAuthorityTaskCompletion, "replay", errStaleTaskCompletion)
	}
	return true, nil
}

func taskCompletionReplayAfterError(
	ctx context.Context,
	store taskCompletionReplayStore,
	worker workergroup.HostPrincipal,
	request workerapi.CompleteTaskRequest,
	completion parsedTaskCompletion,
	operationErr error,
) error {
	replayed, replayErr := taskCompletionWasReplayed(ctx, store, worker, request, completion)
	if replayed {
		return nil
	}
	if errors.Is(replayErr, errStaleTaskCompletion) {
		return replayErr
	}
	if replayErr != nil {
		return errors.Join(operationErr, fmt.Errorf("check task completion replay: %w", replayErr))
	}
	return operationErr
}

func staleTaskCompletion(err error) error {
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) || errors.Is(err, errStaleRunFinalization) {
		return errStaleTaskCompletion
	}
	return err
}
