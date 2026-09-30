package command

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// RecoveryCandidate is a Command whose physical authority may be lost, at the
// revision its discovery observed.
type RecoveryCandidate struct {
	OrgID            uuid.UUID
	CommandID        uuid.UUID
	ComputerID       uuid.UUID
	ExpectedRevision int64
}

// Recover settles a Command's receipt in its own transaction. It stops a
// Command whose Secret resolutions were revoked, records a lost result when
// its Instance was lost, and reconciles the process scope only on physical
// exclusion evidence (a reclaimed Instance); a member cannot save or close
// its Instance. A candidate whose Command or placement changed returns
// ErrChanged.
func Recover(ctx context.Context, txb db.TxBeginner, candidate RecoveryCandidate) error {
	return changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		return recoverCommand(ctx, tx, candidate)
	}))
}

func recoverCommand(ctx context.Context, tx pgx.Tx, candidate RecoveryCandidate) error {
	q := db.New(tx)
	orgID, commandID, computerID := pgvalue.UUID(candidate.OrgID), pgvalue.UUID(candidate.CommandID), pgvalue.UUID(candidate.ComputerID)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: orgID, CommandID: commandID})
	if err != nil {
		return err
	}
	if target.ComputerID != computerID {
		return ErrChanged
	}
	secretsValid, err := lockRecoverySecrets(ctx, q, commandID, computerID)
	if err != nil {
		return err
	}
	locked, err := lockCommandInstance(ctx, tx, target, commandID)
	if err != nil {
		return err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: commandID})
	if err != nil {
		return err
	}
	if command.Revision != candidate.ExpectedRevision {
		return ErrChanged
	}
	if command.ProcessReconciledAt.Valid {
		return nil
	}
	if !secretsValid && !command.TerminalAt.Valid && !command.CancelRequestedAt.Valid {
		command, err = q.StopSecretRevokedComputerCommand(ctx, db.StopSecretRevokedComputerCommandParams{EnvironmentID: target.EnvironmentID, CommandID: command.ID, ExpectedRevision: command.Revision})
		if err != nil {
			return err
		}
	}
	if !command.ComputerInstanceID.Valid {
		return nil
	}
	instance := locked.Instance()
	if !locked.Bound() || instance.ID != command.ComputerInstanceID || instance.WriterGeneration != command.WriterGeneration.Int64 {
		return ErrChanged
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
			return err
		}
	}
	if instance.ReclaimedAt.Valid && command.TerminalAt.Valid {
		if _, err = q.ReconcileComputerCommand(ctx, db.ReconcileComputerCommandParams{CommandID: command.ID, ComputerInstanceID: instance.ID, WriterGeneration: command.WriterGeneration}); err != nil {
			return err
		}
	}
	return nil
}

// lockRecoverySecrets locks the Command's Secret placements and reports
// whether every one still resolves for this Command at its Secret's current
// revocation generation.
func lockRecoverySecrets(ctx context.Context, q *db.Queries, commandID, computerID pgtype.UUID) (bool, error) {
	rows, err := q.LockProcessSecretDelivery(ctx, db.LockProcessSecretDeliveryParams{CommandID: commandID, ComputerID: computerID})
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
			row.ResolutionCommandID != commandID ||
			!row.ResolutionSecretVersionID.Valid ||
			!row.ResolutionRevocationGeneration.Valid ||
			row.ResolutionRevocationGeneration.Int64 != row.Secret.RevocationGeneration {
			return false, nil
		}
	}
	return true, nil
}
