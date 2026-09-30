package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type commandClaim struct {
	OrgID, CommandID, ComputerInstanceID pgtype.UUID
	WriterGeneration                     int64
}

// instance addresses the Instance the claimed Command is bound to on the
// principal's host epoch.
func (c commandClaim) instance(target db.GetComputerCommandTargetRow, worker workergroup.HostPrincipal) computer.CommandInstanceRef {
	return computer.CommandInstanceRef{
		EnvironmentID: pgvalue.MustUUIDValue(target.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(target.ComputerID),
		CommandID: pgvalue.MustUUIDValue(c.CommandID), InstanceID: pgvalue.MustUUIDValue(c.ComputerInstanceID),
		Host:             computer.Host{GroupID: worker.GroupID, HostID: worker.HostID, Epoch: worker.Epoch},
		WriterGeneration: c.WriterGeneration,
	}
}

type commandClaimAuthority struct {
	Command  db.ComputerCommand
	Instance db.ComputerInstance
	Secrets  []secret.DeliveryEnvelope
}

// The transaction orders Secret, Worker, Computer, Instance and member locks.
// Starting a Command neither replaces the physical writer nor suspends peers.
func claimCommand(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request commandClaim) (commandClaimAuthority, error) {
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
	locked, err := workergroup.LockHost(ctx, q, worker)
	if err != nil {
		return commandClaimAuthority{}, err
	}
	i, err := computer.LockInstanceForCommand(ctx, tx, request.instance(target, worker))
	if err != nil {
		return commandClaimAuthority{}, err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: request.CommandID})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if command.ComputerInstanceID != i.ID || !command.WriterGeneration.Valid || command.WriterGeneration.Int64 != i.WriterGeneration {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	switch command.Status {
	case "starting":
		if i.AdmissionState != "open" || locked.Group.Status != db.WorkerGroupStatusActive || locked.Host.Status != db.WorkerHostStatusActive {
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
	authorized, err := q.CommandLogProducerStillAuthorized(ctx, db.CommandLogProducerStillAuthorizedParams{WorkerHostID: i.WorkerHostID, WorkerGroupID: i.WorkerGroupID, WorkerEpoch: worker.Epoch, ExpiresAt: i.WriterExpiresAt})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if !authorized.Valid || !authorized.Bool {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Instance = i
	return result, nil
}
