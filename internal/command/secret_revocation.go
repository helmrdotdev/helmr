package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
)

// StopSecretRevokedCommands stops up to limit live Commands that resolved
// the revoked Secret at an older generation, oldest first. Each Command is
// fenced in its own transaction: it locks the Command's Computer's Secrets,
// and when that Computer still places the Secret at the revoked generation,
// the Computer, its Instance and the Command, and stops the Command unless
// it is terminal or already stopping. A fenced Command is then recovered in
// a separate transaction; a recovery that finds the Command changed is not a
// failure. It returns the number of candidates examined, including those it
// left unchanged; on the first failure it returns the candidates examined
// before it with the error.
func StopSecretRevokedCommands(
	ctx context.Context,
	txdb db.TxDB,
	revocation secret.Revocation,
	limit int32,
) (int, error) {
	if err := revocation.Validate(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, errors.New("secret revocation batch limit must be positive")
	}
	candidates, err := db.New(txdb).ListSecretRevocationProcesses(
		ctx,
		db.ListSecretRevocationProcessesParams{
			SecretID:             pgvalue.UUID(revocation.SecretID),
			RevocationGeneration: revocation.Generation,
			EnvironmentID:        pgvalue.UUID(revocation.EnvironmentID),
			RowLimit:             limit,
		},
	)
	if err != nil {
		return 0, fmt.Errorf("list secret-revoked process candidates: %w", err)
	}
	examined := 0
	for _, candidate := range candidates {
		if err := stopSecretRevokedCommand(ctx, txdb, candidate, revocation); err != nil {
			return examined, err
		}
		examined++
	}
	return examined, nil
}

func stopSecretRevokedCommand(
	ctx context.Context,
	txdb db.TxDB,
	candidate db.ListSecretRevocationProcessesRow,
	revocation secret.Revocation,
) error {
	var revision int64
	fenced := false
	err := db.RunTx(ctx, txdb, func(tx pgx.Tx) error {
		var err error
		revision, fenced, err = fenceSecretRevokedCommand(ctx, tx, candidate, revocation)
		return err
	})
	if err != nil || !fenced {
		return err
	}
	err = Recover(ctx, txdb, RecoveryCandidate{
		OrgID:            pgvalue.MustUUIDValue(candidate.OrgID),
		CommandID:        pgvalue.MustUUIDValue(candidate.ID),
		ComputerID:       pgvalue.MustUUIDValue(candidate.ComputerID),
		ExpectedRevision: revision,
	})
	if err != nil && !errors.Is(err, ErrChanged) {
		return fmt.Errorf("recover secret-revoked Command: %w", err)
	}
	return nil
}

// fenceSecretRevokedCommand stops the candidate Command in tx and returns
// its resulting revision. It reports false, leaving the Command unchanged,
// when the Computer no longer places the Secret at the revoked generation or
// the Command no longer exists.
func fenceSecretRevokedCommand(
	ctx context.Context,
	tx pgx.Tx,
	candidate db.ListSecretRevocationProcessesRow,
	revocation secret.Revocation,
) (int64, bool, error) {
	revoked, err := secret.CheckRevocation(
		ctx,
		tx,
		pgvalue.MustUUIDValue(candidate.ComputerID),
		revocation,
	)
	if err != nil || !revoked {
		return 0, false, err
	}
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{
		OrgID: candidate.OrgID, CommandID: candidate.ID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("locate secret-revoked Command: %w", err)
	}
	if target.ComputerID != candidate.ComputerID {
		return 0, false, errors.New("secret-revoked Command placement changed")
	}
	admission, err := computer.LockForAdmission(
		ctx,
		tx,
		pgvalue.MustUUIDValue(target.EnvironmentID),
		pgvalue.MustUUIDValue(target.ComputerID),
	)
	if err != nil {
		return 0, false, fmt.Errorf("lock secret-revoked Computer: %w", err)
	}
	if err := admission.LockLiveInstance(ctx); err != nil {
		return 0, false, fmt.Errorf("lock secret-revoked Instance: %w", err)
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{
		EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: candidate.ID,
	})
	if err != nil {
		return 0, false, fmt.Errorf("lock secret-revoked Command: %w", err)
	}
	if !command.TerminalAt.Valid && !command.CancelRequestedAt.Valid {
		command, err = q.StopSecretRevokedComputerCommand(ctx, db.StopSecretRevokedComputerCommandParams{
			EnvironmentID: target.EnvironmentID, CommandID: command.ID, ExpectedRevision: command.Revision,
		})
		if err != nil {
			return 0, false, fmt.Errorf("fail secret-revoked Command: %w", err)
		}
	}
	return command.Revision, true, nil
}
