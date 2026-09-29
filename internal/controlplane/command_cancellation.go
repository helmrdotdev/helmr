package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"

	"github.com/jackc/pgx/v5"
)

// Cancellation grants no launch or Secret authority and never changes physical state.
func claimCommandCancellation(ctx context.Context, tx pgx.Tx, worker workerActor, request commandClaim) (commandClaimAuthority, error) {
	q := db.New(tx)
	result := commandClaimAuthority{}
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: request.OrgID, CommandID: request.CommandID})
	if err != nil {
		return result, err
	}
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(worker.WorkerGroupID))
	if err != nil {
		return commandClaimAuthority{}, err
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: pgvalue.UUID(worker.WorkerHostID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID)})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if err = worker.checkLockedClaims(host, group); err != nil {
		return commandClaimAuthority{}, err
	}
	computer, err := q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	i, err := q.LockComputerCommandInstance(ctx, db.LockComputerCommandInstanceParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: request.CommandID})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: request.CommandID})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if i.ID != request.ComputerInstanceID || i.WorkerHostID != host.ID || i.WorkerGroupID != group.ID || i.WorkerEpoch != worker.WorkerEpoch || i.WriterGeneration != request.WriterGeneration || command.ComputerInstanceID != i.ID || !command.WriterGeneration.Valid || command.WriterGeneration.Int64 != i.WriterGeneration {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	if computer.Status != "active" || computer.DesiredState != "active" || i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.WriterGeneration != computer.WriterGeneration || (i.AdmissionState != "open" && i.AdmissionState != "draining") {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	if command.Status != "stopping" || !command.CancelRequestedAt.Valid || command.TerminalAt.Valid {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Command = command
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{WorkerHostID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: worker.WorkerEpoch, ExpiresAt: i.WriterExpiresAt})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if !authorized.Valid || !authorized.Bool {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Instance = i
	return result, nil
}
