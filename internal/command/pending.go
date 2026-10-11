package command

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Pending is a pending Command at the revision its discovery observed.
type Pending struct {
	EnvironmentID    uuid.UUID
	CommandID        uuid.UUID
	ExpectedRevision int64
}

// Failure is the terminal reason code and JSON error document a pending
// Command fails with.
type Failure struct {
	Code   string
	Detail []byte
}

// FailPending fails a pending Command that was never bound to an Instance,
// in the caller's transaction, which the caller commits. It locks the
// Computer and the Command's bound Instance before the Command, so a
// candidate that was bound after discovery locks that Instance before it is
// rejected. A Command that is missing, changed revision or is no longer
// pending and unbound returns ErrChanged.
func FailPending(ctx context.Context, tx pgx.Tx, pending Pending, failure Failure) error {
	return changed(failPending(ctx, tx, pending, failure))
}

func failPending(ctx context.Context, tx pgx.Tx, pending Pending, failure Failure) error {
	q := db.New(tx)
	commandID := pgvalue.UUID(pending.CommandID)
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{EnvironmentID: pgvalue.UUID(pending.EnvironmentID), CommandID: commandID})
	if err != nil {
		return err
	}
	if _, err = lockCommandLease(ctx, tx, target, pending.CommandID); err != nil {
		return err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: commandID})
	if err != nil {
		return err
	}
	if command.Revision != pending.ExpectedRevision {
		return ErrChanged
	}
	if command.Status != "pending" || command.ComputerLeaseEpoch.Valid {
		return ErrChanged
	}
	_, err = tx.Exec(ctx, `UPDATE computer_commands SET status='failed',failure_reason='dispatch_failed',error=$3,
 terminal_at=clock_timestamp(),terminal_reason_code=$4,result_expires_at=clock_timestamp()+interval '30 days',revision=revision+1,updated_at=clock_timestamp()
 WHERE environment_id=$1 AND id=$2`, target.EnvironmentID, commandID, failure.Detail, failure.Code)
	return err
}
