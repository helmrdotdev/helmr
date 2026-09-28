package run

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrExecutionTargetNotFound = errors.New("execution target not found")

// LockLiveExecutionForSession locks the addressed Session together with the
// source lineage. Reciprocal sends acquire Computers and Sessions in UUID order.
// Secret locks, when needed, must precede this operation; the caller owns the tx.
func LockLiveExecutionForSession(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (ExecutionAuthority, error) {
	if !target.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{session: target})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if a.Lease.Status != db.RunLeaseStatusRunning {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	return a, nil
}

type executionTarget struct{ computer, session pgtype.UUID }

func LockLiveExecutionForComputer(ctx context.Context, tx pgx.Tx, fence ExecutionFence, target pgtype.UUID) (ExecutionAuthority, error) {
	if !target.Valid {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	a, err := lockExecution(ctx, tx, fence, executionLive, executionTarget{computer: target})
	if err != nil {
		return ExecutionAuthority{}, err
	}
	if a.Lease.Status != db.RunLeaseStatusRunning {
		return ExecutionAuthority{}, pgx.ErrNoRows
	}
	return a, nil
}

func lockExecutionPlacement(ctx context.Context, tx pgx.Tx, lineage []uuid.UUID, environment pgtype.UUID, target executionTarget) error {
	if !target.session.Valid && !target.computer.Valid {
		return lockCancellationPlacement(ctx, tx, lineage, nil)
	}
	var computer pgtype.UUID
	if target.session.Valid {
		if err := tx.QueryRow(ctx, `SELECT computer_id FROM sessions WHERE environment_id=$1 AND id=$2`, environment, target.session).Scan(&computer); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrExecutionTargetNotFound
			}
			return err
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT id FROM computers WHERE environment_id=$1 AND id=$2`, environment, target.computer).Scan(&computer); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrExecutionTargetNotFound
			}
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id FROM computers WHERE id=$2 OR id IN
 (SELECT computer_id FROM runs WHERE id=ANY($1::uuid[])) ORDER BY id FOR UPDATE`, pgUUIDs(lineage), computer)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	_, err = db.New(tx).LockCancellationInstances(ctx, pgUUIDs(lineage))
	return err
}

func lockExecutionSessions(ctx context.Context, tx pgx.Tx, scope CancellationRequest, lineage []uuid.UUID, target pgtype.UUID) error {
	if !target.Valid {
		return lockCancellationActors(ctx, tx, scope, lineage)
	}
	rows, err := tx.Query(ctx, `SELECT s.id FROM sessions s
 WHERE s.environment_id=$1 AND (s.id=$2 OR s.id IN
 (SELECT session_id FROM runs WHERE id=ANY($3::uuid[]))) ORDER BY s.id FOR UPDATE`, pgvalue.UUID(scope.EnvironmentID), target, pgUUIDs(lineage))
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var id pgtype.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		found = found || id == target
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if !found {
		return ErrExecutionTargetNotFound
	}
	return nil
}
