package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type commandClaim struct {
	OrgID, CommandID, ComputerInstanceID pgtype.UUID
	WriterGeneration                     int64
}

type commandClaimAuthority struct {
	Command  db.ComputerCommand
	Instance db.ComputerInstance
	Secrets  []secret.DeliveryEnvelope
}

// The transaction orders Secret, Worker, Computer, Instance and member locks.
// Starting a Command neither replaces the physical writer nor suspends peers.
func claimCommand(ctx context.Context, tx pgx.Tx, worker workerActor, request commandClaim) (commandClaimAuthority, error) {
	q := db.New(tx)
	result := commandClaimAuthority{}
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: request.OrgID, CommandID: request.CommandID})
	if err != nil {
		return result, err
	}
	result.Secrets, err = secret.LockProcessDelivery(ctx, q, request.CommandID, target.ComputerID)
	if err != nil {
		return commandClaimAuthority{}, err
	}
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(worker.WorkerGroupID))
	if err != nil {
		return commandClaimAuthority{}, err
	}
	host, err := q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{ID: pgvalue.UUID(worker.WorkerHostID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID)})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if group.ClaimVersion != worker.GroupClaimVersion || host.ClaimVersion != worker.ClaimVersion {
		return commandClaimAuthority{}, pgx.ErrNoRows
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
	if computer.Status != "active" || computer.DesiredState != "active" || i.ReclaimedAt.Valid || i.DesiredState != "ready" || i.ObservedState != "ready" || i.ObservedDesiredVersion != i.DesiredVersion || i.MountState != "mounted" || i.WriterGeneration != computer.WriterGeneration {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	switch command.Status {
	case "starting":
		if i.AdmissionState != "open" || group.Status != db.WorkerGroupStatusActive || host.Status != db.WorkerHostStatusActive {
			return commandClaimAuthority{}, pgx.ErrNoRows
		}
	case "running":
		// A lost claim response can be retried while existing work drains.
		if i.AdmissionState != "open" && i.AdmissionState != "draining" {
			return commandClaimAuthority{}, pgx.ErrNoRows
		}
	default:
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Command, err = q.StartComputerCommand(ctx, db.StartComputerCommandParams{CommandID: command.ID, ComputerInstanceID: i.ID, WriterGeneration: command.WriterGeneration})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{WorkerHostID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: worker.WorkerEpoch, WorkerClaimVersion: worker.ClaimVersion, GroupClaimVersion: worker.GroupClaimVersion, ExpiresAt: i.WriterExpiresAt})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if !authorized.Valid || !authorized.Bool {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Instance = i
	return result, nil
}
