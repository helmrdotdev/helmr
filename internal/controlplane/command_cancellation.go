package controlplane

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workergroup"

	"github.com/jackc/pgx/v5"
)

// Cancellation grants no launch or Secret authority and never changes physical state.
func claimCommandCancellation(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request commandClaim) (commandClaimAuthority, error) {
	q := db.New(tx)
	result := commandClaimAuthority{}
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: request.OrgID, CommandID: request.CommandID})
	if err != nil {
		return result, err
	}
	if _, err := workergroup.LockHost(ctx, q, worker); err != nil {
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
	if command.ComputerInstanceID != i.ID || !command.WriterGeneration.Valid || command.WriterGeneration.Int64 != i.WriterGeneration || (i.AdmissionState != "open" && i.AdmissionState != "draining") {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	if command.Status != "stopping" || !command.CancelRequestedAt.Valid || command.TerminalAt.Valid {
		return commandClaimAuthority{}, pgx.ErrNoRows
	}
	result.Command = command
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
