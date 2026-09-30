package command

import (
	"context"
	"slices"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ClaimRequest asks for the next action on the Instance incarnation and
// writer generation a worker host acts for. The host lists the Commands it
// already runs and the cancellations it already delivers, which are skipped.
type ClaimRequest struct {
	OrgID                 uuid.UUID
	EnvironmentID         uuid.UUID
	InstanceID            uuid.UUID
	WriterGeneration      int64
	ActiveCommandIDs      []uuid.UUID
	ActiveCancellationIDs []uuid.UUID
}

// ClaimResult is at most one action for the worker host: a Command to
// start, a cancellation to deliver, or a terminal Command whose process
// scope to release. All nil means there is nothing to do.
type ClaimResult struct {
	Start        *Start
	Cancellation *Cancellation
	Release      *Release
}

// Start grants launching a Command, or replaying its launch after a lost
// claim response. The caller opens the Secret envelopes and reads the
// protected environment after the claim committed.
type Start struct {
	Command            db.ComputerCommand
	Instance           db.ComputerInstance
	Secrets            []secret.DeliveryEnvelope
	RequestFingerprint []byte
}

// Cancellation asks the worker host to stop a running Command. It grants no
// launch or Secret authority and never changes physical state.
type Cancellation struct {
	CommandID          uuid.UUID
	ComputerID         uuid.UUID
	InstanceID         uuid.UUID
	WriterGeneration   int64
	RequestFingerprint []byte
	ExpiresAt          time.Time
}

// Release is the recorded completion of a terminal Command that the worker
// host no longer runs. The claim has already recorded it; the host replays
// it to reconcile the process scope.
type Release struct {
	ComputerID         uuid.UUID
	RequestFingerprint []byte
	Completion         CompletionReport
}

// Claim selects and grants the first actionable Command bound to the
// Instance: a requested cancellation, then the release of a terminal Command
// whose result is safe to release, then a Command to start. The claim runs
// in one transaction.
func Claim(ctx context.Context, txb db.TxBeginner, worker workergroup.HostPrincipal, request ClaimRequest) (ClaimResult, error) {
	var result ClaimResult
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		var err error
		result, err = claimNext(ctx, tx, worker, request)
		return err
	})
	if err != nil {
		return ClaimResult{}, changed(err)
	}
	return result, nil
}

func claimNext(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request ClaimRequest) (ClaimResult, error) {
	q := db.New(tx)
	org := pgvalue.UUID(request.OrgID)
	i, err := q.GetComputerInstance(ctx, db.GetComputerInstanceParams{ID: pgvalue.UUID(request.InstanceID), EnvironmentID: pgvalue.UUID(request.EnvironmentID)})
	if err != nil {
		return ClaimResult{}, err
	}
	if i.OrgID != org || i.WorkerHostID != pgvalue.UUID(worker.HostID) || i.WorkerGroupID != pgvalue.UUID(worker.GroupID) || i.WorkerEpoch != worker.Epoch || i.WriterGeneration != request.WriterGeneration {
		return ClaimResult{}, pgx.ErrNoRows
	}
	commands, err := q.ListInstanceCommands(ctx, db.ListInstanceCommandsParams{ComputerInstanceID: i.ID, WriterGeneration: pgtype.Int8{Int64: request.WriterGeneration, Valid: true}})
	if err != nil {
		return ClaimResult{}, err
	}
	for _, candidate := range commands {
		id := uuid.UUID(candidate.ID.Bytes)
		claim := commandClaim{OrgID: org, CommandID: candidate.ID, ComputerInstanceID: i.ID, WriterGeneration: request.WriterGeneration}
		if candidate.Status == "stopping" && !slices.Contains(request.ActiveCancellationIDs, id) {
			receipt, err := q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
			if err != nil {
				return ClaimResult{}, err
			}
			grant, err := claimCommandCancellation(ctx, tx, worker, claim)
			if err != nil {
				return ClaimResult{}, err
			}
			return ClaimResult{Cancellation: &Cancellation{
				CommandID: id, ComputerID: uuid.UUID(candidate.ComputerID.Bytes), InstanceID: uuid.UUID(i.ID.Bytes),
				WriterGeneration: grant.Instance.WriterGeneration, RequestFingerprint: receipt.RequestFingerprint, ExpiresAt: grant.Instance.WriterExpiresAt.Time,
			}}, nil
		}
		if slices.Contains(request.ActiveCommandIDs, id) {
			continue
		}
		if candidate.TerminalAt.Valid && !candidate.ProcessReconciledAt.Valid {
			receipt, err := q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
			if err != nil {
				return ClaimResult{}, err
			}
			report, err := release(candidate, request.OrgID)
			if err != nil {
				return ClaimResult{}, err
			}
			if report != nil {
				p, err := report.parse()
				if err != nil {
					return ClaimResult{}, err
				}
				if err = applyCompletion(ctx, tx, worker, p, report.WriterGeneration, false); err != nil {
					return ClaimResult{}, err
				}
				return ClaimResult{Release: &Release{ComputerID: uuid.UUID(candidate.ComputerID.Bytes), RequestFingerprint: receipt.RequestFingerprint, Completion: *report}}, nil
			}
		}
		if candidate.Status != "starting" && candidate.Status != "running" {
			continue
		}
		receipt, err := q.GetIdempotencyClaim(ctx, db.GetIdempotencyClaimParams{EnvironmentID: candidate.EnvironmentID, ID: candidate.ClaimID})
		if err != nil {
			return ClaimResult{}, err
		}
		authority, err := claimCommand(ctx, tx, worker, claim)
		if err != nil {
			return ClaimResult{}, err
		}
		return ClaimResult{Start: &Start{Command: authority.Command, Instance: authority.Instance, Secrets: authority.Secrets, RequestFingerprint: receipt.RequestFingerprint}}, nil
	}
	return ClaimResult{}, nil
}

// commandClaim addresses a Command bound to the Instance incarnation at the
// writer generation the claiming host acts for.
type commandClaim struct {
	OrgID, CommandID, ComputerInstanceID pgtype.UUID
	WriterGeneration                     int64
}

// commandClaimAuthority is what a claim grants inside its transaction.
type commandClaimAuthority struct {
	Command  db.ComputerCommand
	Instance db.ComputerInstance
	Secrets  []secret.DeliveryEnvelope
}

// instance addresses the Instance the claimed Command is bound to on the
// principal's host epoch.
func (c commandClaim) instance(target db.GetComputerCommandTargetRow, worker workergroup.HostPrincipal) computer.CommandInstanceRef {
	return computer.CommandInstanceRef{
		EnvironmentID: pgvalue.MustUUIDValue(target.EnvironmentID), ComputerID: pgvalue.MustUUIDValue(target.ComputerID),
		CommandID: pgvalue.MustUUIDValue(c.CommandID), InstanceID: pgvalue.MustUUIDValue(c.ComputerInstanceID),
		Host:             host(worker),
		WriterGeneration: c.WriterGeneration,
	}
}

// claimCommand starts a Command. The transaction orders Secret, Worker,
// Computer, Instance and member locks. Starting a Command neither replaces
// the physical writer nor suspends peers.
func claimCommand(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request commandClaim) (commandClaimAuthority, error) {
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: request.OrgID, CommandID: request.CommandID})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	secrets, err := secret.LockProcessDelivery(ctx, q, request.CommandID, target.ComputerID)
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
	started, err := q.StartComputerCommand(ctx, db.StartComputerCommandParams{CommandID: command.ID, ComputerInstanceID: i.ID, WriterGeneration: command.WriterGeneration})
	if err != nil {
		return commandClaimAuthority{}, err
	}
	if err = producerStillAuthorized(ctx, q, worker, i); err != nil {
		return commandClaimAuthority{}, err
	}
	return commandClaimAuthority{Command: started, Instance: i, Secrets: secrets}, nil
}

// claimCommandCancellation grants delivering a requested cancellation. It
// grants no launch or Secret authority and never changes physical state.
func claimCommandCancellation(ctx context.Context, tx pgx.Tx, worker workergroup.HostPrincipal, request commandClaim) (commandClaimAuthority, error) {
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: request.OrgID, CommandID: request.CommandID})
	if err != nil {
		return commandClaimAuthority{}, err
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
	if err = producerStillAuthorized(ctx, q, worker, i); err != nil {
		return commandClaimAuthority{}, err
	}
	return commandClaimAuthority{Command: command, Instance: i}, nil
}
