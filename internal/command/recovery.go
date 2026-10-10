package command

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

type RecoveryCandidate struct {
	EnvironmentID    uuid.UUID
	CommandID        uuid.UUID
	ComputerID       uuid.UUID
	ExpectedRevision int64
}

// Recover never rebinds or replays execution. Losing authority settles the logical
// result; only a recorded physical fence reconciles the historical process scope.
func Recover(ctx context.Context, txb db.TxBeginner, candidate RecoveryCandidate) error {
	err := changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error { return recoverCommand(ctx, tx, candidate) }))
	if err != nil {
		return fmt.Errorf("recover Command: %w", err)
	}
	return nil
}
func recoverCommand(ctx context.Context, tx pgx.Tx, candidate RecoveryCandidate) error {
	q := db.New(tx)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{EnvironmentID: pgvalue.UUID(candidate.EnvironmentID), CommandID: pgvalue.UUID(candidate.CommandID)})
	if err != nil {
		return err
	}
	env := uuid.UUID(target.EnvironmentID.Bytes)
	if target.ComputerID != pgvalue.UUID(candidate.ComputerID) {
		return ErrChanged
	}
	bindings, err := lockBindings(ctx, tx, env, candidate.ComputerID)
	if err != nil {
		return err
	}
	l, err := lockCommandLease(ctx, tx, target, candidate.CommandID)
	if err != nil {
		return err
	}
	c, err := lockCommand(ctx, tx, env, candidate.ComputerID, candidate.CommandID)
	if err != nil {
		return err
	}
	if c.Revision != candidate.ExpectedRevision {
		return ErrChanged
	}
	if c.ProcessReconciledAt.Valid {
		return nil
	}
	revoked := false
	for _, b := range bindings {
		if b.status != "active" {
			revoked = true
		}
	}
	if revoked && !c.TerminalAt.Valid && !c.CancelRequestedAt.Valid {
		if _, err = tx.Exec(ctx, `UPDATE computer_commands SET cancel_requested_at=clock_timestamp(),
 status=CASE WHEN computer_lease_epoch IS NULL THEN 'failed' ELSE 'stopping' END,
 failure_reason=CASE WHEN computer_lease_epoch IS NULL THEN 'guest_failure' END,
 error=CASE WHEN computer_lease_epoch IS NULL THEN '{"code":"secret_revoked","retryable":false}'::jsonb END,
 terminal_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp() END,
 terminal_reason_code=CASE WHEN computer_lease_epoch IS NULL THEN 'secret_revoked' END,
 result_expires_at=CASE WHEN computer_lease_epoch IS NULL THEN clock_timestamp()+interval '30 days' END,
 revision=revision+1,updated_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, env, candidate.CommandID); err != nil {
			return err
		}
	}
	if !c.ComputerLeaseEpoch.Valid {
		return nil
	}
	if l.Epoch != c.ComputerLeaseEpoch.Int64 {
		return ErrChanged
	}
	var lost bool
	if err = tx.QueryRow(ctx, `SELECT status IN ('lost','released') OR fenced_at IS NOT NULL OR expires_at<=clock_timestamp()
 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, candidate.ComputerID, l.Epoch).Scan(&lost); err != nil {
		return err
	}
	if lost && !c.TerminalAt.Valid {
		if _, err = tx.Exec(ctx, `UPDATE computer_commands SET status='lost',failure_reason='guest_failure',terminal_reason_code='computer_lease_lost',
 error='{"code":"computer_lease_lost","retryable":false}'::jsonb,terminal_at=clock_timestamp(),result_expires_at=clock_timestamp()+interval '30 days',revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND terminal_at IS NULL`, env, candidate.CommandID); err != nil {
			return err
		}
	}
	if l.FencedAt != nil {
		_, err = tx.Exec(ctx, `UPDATE computer_commands SET process_reconciled_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND terminal_at IS NOT NULL AND process_reconciled_at IS NULL`, env, candidate.CommandID)
	}
	return err
}
