package command

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

// Pending is a pending Command at the revision its discovery observed.
type Pending struct {
	OrgID            uuid.UUID
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
	target, err := q.GetComputerCommandTarget(ctx, db.GetComputerCommandTargetParams{OrgID: pgvalue.UUID(pending.OrgID), CommandID: commandID})
	if err != nil {
		return err
	}
	if _, err = lockCommandInstance(ctx, tx, target, commandID); err != nil {
		return err
	}
	command, err := q.LockComputerCommand(ctx, db.LockComputerCommandParams{EnvironmentID: target.EnvironmentID, ComputerID: target.ComputerID, CommandID: commandID})
	if err != nil {
		return err
	}
	if command.Revision != pending.ExpectedRevision {
		return ErrChanged
	}
	_, err = q.FailPendingComputerCommand(ctx, db.FailPendingComputerCommandParams{
		ReasonCode: pgvalue.Text(failure.Code), Error: failure.Detail,
		EnvironmentID: target.EnvironmentID, CommandID: commandID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrChanged
		}
		return fmt.Errorf("fail pending Command: %w", err)
	}
	return nil
}
