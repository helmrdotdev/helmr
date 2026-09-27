package dispatch

import (
	"context"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type RecoverableComputerCommandCandidate struct {
	OrgID            pgtype.UUID
	CommandID        pgtype.UUID
	ComputerID       pgtype.UUID
	ExpectedRevision int64
}

// Recovery settles a Command's receipt. Only physical exclusion evidence can
// reconcile an uncertain process; a member cannot save or close its Instance.
func (d *Authority) RecoverComputerCommand(ctx context.Context, candidate RecoverableComputerCommandCandidate) error {
	tx, err := d.begin(ctx)
	if err != nil {
		return fmt.Errorf("begin Command recovery: %w", err)
	}
	defer rollback(ctx, tx)
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: candidate.OrgID, CommandID: candidate.CommandID})
	if err != nil {
		return classifyComputerCommandRecoveryError(err)
	}
	if target.ComputerID != candidate.ComputerID {
		return ErrCandidateChanged
	}
	secretsValid, err := lockComputerCommandRecoverySecrets(ctx, q, candidate)
	if err != nil {
		return err
	}
	if _, err = q.LockComputer(ctx, db.LockComputerParams{EnvironmentID: target.EnvironmentID, ID: target.ComputerID}); err != nil {
		return classifyComputerCommandRecoveryError(err)
	}
	instance, instanceErr := q.LockComputerCommandInstance(ctx, db.LockComputerCommandInstanceParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.CommandID})
	if instanceErr != nil && !errors.Is(instanceErr, pgx.ErrNoRows) {
		return instanceErr
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.CommandID})
	if err != nil {
		return classifyComputerCommandRecoveryError(err)
	}
	if command.Revision != candidate.ExpectedRevision {
		return ErrCandidateChanged
	}
	if command.ProcessReconciledAt.Valid {
		return tx.Commit(ctx)
	}
	if !secretsValid && !command.TerminalAt.Valid && !command.CancelRequestedAt.Valid {
		command, err = q.StopSecretRevokedComputerCommand(ctx, db.StopSecretRevokedComputerCommandParams{EnvironmentID: target.EnvironmentID, CommandID: command.ID, ExpectedRevision: command.Revision})
		if err != nil {
			return classifyComputerCommandRecoveryError(err)
		}
	}
	if !command.ComputerInstanceID.Valid {
		return tx.Commit(ctx)
	}
	if instanceErr != nil || instance.ID != command.ComputerInstanceID || instance.WriterGeneration != command.WriterGeneration.Int64 {
		return ErrCandidateChanged
	}
	now, err := q.GetRunLeaseRenewalTime(ctx)
	if err != nil {
		return err
	}
	lost := instance.ReclaimedAt.Valid || instance.ObservedState == "failed" || instance.ObservedState == "lost" ||
		!instance.WriterExpiresAt.Time.After(now.Time) || instance.MountState == "lost" || instance.MountState == "failed"
	if lost && !command.TerminalAt.Valid {
		command, err = q.CompleteComputerCommand(ctx, db.CompleteComputerCommandParams{
			CommandID: command.ID, ComputerInstanceID: instance.ID, WriterGeneration: command.WriterGeneration,
			Status: "lost", FailureReason: pgvalue.Text("guest_failure"), ReasonCode: pgvalue.Text("computer_instance_lost"),
			Error: []byte(`{"code":"computer_instance_lost","retryable":false}`),
		})
		if err != nil {
			return classifyComputerCommandRecoveryError(err)
		}
	}
	if instance.ReclaimedAt.Valid && command.TerminalAt.Valid {
		if _, err = q.ReconcileComputerCommand(ctx, db.ReconcileComputerCommandParams{CommandID: command.ID, ComputerInstanceID: instance.ID, WriterGeneration: command.WriterGeneration}); err != nil {
			return classifyComputerCommandRecoveryError(err)
		}
	}
	return tx.Commit(ctx)
}

func lockComputerCommandRecoverySecrets(
	ctx context.Context,
	q *db.Queries,
	candidate RecoverableComputerCommandCandidate,
) (bool, error) {
	rows, err := q.LockProcessSecretDelivery(
		ctx,
		db.LockProcessSecretDeliveryParams{
			CommandID:  candidate.CommandID,
			ComputerID: candidate.ComputerID,
		},
	)
	if err != nil {
		return false, err
	}
	if len(rows) > 64 {
		return false, errors.New("computer secret placements exceed their bound")
	}
	for _, row := range rows {
		if row.Secret.Status != "active" ||
			!row.ResolutionID.Valid ||
			!row.ResolutionCommandID.Valid ||
			row.ResolutionCommandID != candidate.CommandID ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 !=
				row.Secret.RevocationGeneration {
			return false, nil
		}
	}
	return true, nil
}

func classifyComputerCommandRecoveryError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCandidateChanged
	}
	return err
}
