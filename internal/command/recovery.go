package command

import (
	"context"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
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
func Recover(ctx context.Context, auditPolicy auditpolicy.Policy, txb db.TxBeginner, candidate RecoveryCandidate) error {
	err := changed(db.RunTx(ctx, txb, func(tx pgx.Tx) error { return recoverCommand(ctx, auditPolicy, tx, candidate) }))
	if err != nil {
		return fmt.Errorf("recover Command: %w", err)
	}
	return nil
}
func recoverCommand(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, candidate RecoveryCandidate) error {
	q := db.New(tx)
	target, err := q.GetCommandTarget(ctx, db.GetCommandTargetParams{EnvironmentID: pgvalue.UUID(candidate.EnvironmentID), CommandID: pgvalue.UUID(candidate.CommandID)})
	if err != nil {
		return err
	}
	env := uuid.UUID(target.EnvironmentID.Bytes)
	if target.ComputerID != pgvalue.UUID(candidate.ComputerID) {
		return ErrChanged
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
	if !c.ComputerLeaseEpoch.Valid {
		return nil
	}
	if l.Epoch != c.ComputerLeaseEpoch.Int64 {
		return ErrChanged
	}
	var lost bool
	if err = tx.QueryRow(ctx, `SELECT status='lost' OR fenced_at IS NOT NULL OR expires_at<=clock_timestamp()
 FROM computer_leases WHERE environment_id=$1 AND computer_id=$2 AND epoch=$3`, env, candidate.ComputerID, l.Epoch).Scan(&lost); err != nil {
		return err
	}
	if lost && !c.TerminalAt.Valid {
		if _, err = tx.Exec(ctx, `UPDATE commands SET status='lost',failure_reason='guest_failure',terminal_reason_code='computer_lease_lost',
 error='{"code":"computer_lease_lost","retryable":false}'::jsonb,terminal_at=clock_timestamp(),result_expires_at=clock_timestamp()+interval '30 days',revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND terminal_at IS NULL`, env, candidate.CommandID); err != nil {
			return err
		}
		if err = auditServiceFailure(ctx, tx, auditPolicy, c, audit.ControlPlane, "unknown", "worker_lost"); err != nil {
			return err
		}
	}
	if l.FencedAt != nil {
		_, err = tx.Exec(ctx, `UPDATE commands SET process_reconciled_at=clock_timestamp(),revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2 AND terminal_at IS NOT NULL AND process_reconciled_at IS NULL`, env, candidate.CommandID)
	}
	return err
}
