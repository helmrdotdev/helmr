package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var errStaleRunFinalization = errors.New("run finalization authority is stale")

func (s *Server) beginRunFinalization(
	ctx context.Context,
	worker workerActor,
	request workerapi.BeginRunFinalizationRequest,
	parsed parsedRunFinalization,
) (workerapi.BeginRunFinalizationResponse, error) {
	var response workerapi.BeginRunFinalizationResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerHostID: pgvalue.UUID(worker.WorkerHostID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleRunFinalization(err)
		}
		if locators.RunID != pgvalue.UUID(parsed.runID) ||
			locators.AttemptNumber != parsed.attempt {
			return errStaleRunFinalization
		}
		if _, err := secret.LockAttemptDelivery(
			ctx,
			work.q,
			locators.RunID,
			locators.AttemptNumber,
			locators.ComputerID,
		); err != nil {
			return fmt.Errorf("lock run finalization secret authority: %w", err)
		}
		authority, err := run.BeginExecutionFinalization(ctx, work.tx, run.ExecutionFinalization{
			Fence: workerExecutionFence(worker, parsed.lease, request.Lease),
			RunID: pgvalue.UUID(parsed.runID), AttemptNumber: parsed.attempt,
			OperationID: pgvalue.UUID(parsed.operationID), Fingerprint: parsed.fingerprint,
		})
		if errors.Is(err, run.ErrExecutionWorkerClaims) {
			return errStaleWorkerClaims
		}
		if err != nil {
			return staleRunFinalization(err)
		}
		response = workerapi.BeginRunFinalizationResponse{
			Lease:     request.Lease,
			ExpiresAt: authority.Lease.ExpiresAt.Time.UTC(), OperationID: parsed.operationID.String(),
			StartedAt: authority.Lease.FinalizationStartedAt.Time.UTC(),
		}
		return nil
	})
	return response, err
}

func staleRunFinalization(err error) error {
	if err == nil {
		return errStaleRunFinalization
	}
	return errors.Join(errStaleRunFinalization, err)
}
