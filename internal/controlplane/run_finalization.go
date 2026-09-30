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
	"github.com/helmrdotdev/helmr/internal/workergroup"
)

var errStaleRunFinalization = errors.New("run finalization authority is stale")

func (s *Server) beginRunFinalization(
	ctx context.Context,
	worker workergroup.HostPrincipal,
	request workerapi.BeginRunFinalizationRequest,
	parsed parsedRunFinalization,
) (workerapi.BeginRunFinalizationResponse, error) {
	var response workerapi.BeginRunFinalizationResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID),
			WorkerEpoch: worker.Epoch})
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
		if err != nil {
			return staleRunFinalization(err)
		}
		// Guest emits ProgramQuiesced only after the scoped cgroup is empty and
		// output is drained. Persist that authenticated physical proof separately
		// from logical finalization; no other member's process is reconciled.
		if _, err := work.tx.Exec(ctx, `UPDATE run_leases SET process_reconciled_at=COALESCE(process_reconciled_at,clock_timestamp())
 WHERE id=$1 AND computer_instance_id=$2 AND writer_generation=$3`,
			authority.Lease.ID, authority.Lease.ComputerInstanceID, authority.Lease.WriterGeneration); err != nil {
			return fmt.Errorf("record Program quiescence: %w", err)
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
	if errors.Is(err, workergroup.ErrStaleClaims) {
		return err
	}
	if err == nil {
		return errStaleRunFinalization
	}
	return errors.Join(errStaleRunFinalization, err)
}
