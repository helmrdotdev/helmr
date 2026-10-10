package agent

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// LockRuntimeCaller validates current Session execution authority and retains its
// locks until the caller's transaction ends. The authorized operation must use
// this transaction and caller.Execution.EnvironmentID, never a supplied scope.
func LockRuntimeCaller(ctx context.Context, tx pgx.Tx, caller Caller) error {
	if caller.Kind != "session" || caller.ID == uuid.Nil() {
		return ErrDenied
	}
	if err := lockRuntimeHost(ctx, tx, caller); err != nil {
		return err
	}
	owner, err := lockSession(ctx, tx, caller.Execution.EnvironmentID, caller.ID)
	if err != nil {
		return hideMissing(err)
	}
	return authorizeSessionInput(ctx, tx, caller, caller.Execution.EnvironmentID, owner, true)
}

var ErrRootSessionRequired = errors.New("this operation requires a root Session; report the request to its owner")

// Parentage is immutable. Creation depth is fixed at one owned child level.
func requireRootCaller(ctx context.Context, tx pgx.Tx, caller Caller) error {
	var root bool
	if err := tx.QueryRow(ctx, `SELECT parent_session_id IS NULL FROM sessions WHERE environment_id=$1 AND id=$2`, caller.Execution.EnvironmentID, caller.ID).Scan(&root); err != nil {
		return hideMissing(err)
	}
	if !root {
		return ErrRootSessionRequired
	}
	return nil
}

func requireOwnedTarget(ctx context.Context, tx pgx.Tx, env, caller, target uuid.UUID) error {
	var owned bool
	if err := tx.QueryRow(ctx, `WITH RECURSIVE ancestors AS (
 SELECT parent_session_id FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.parent_session_id FROM sessions s JOIN ancestors a ON s.id=a.parent_session_id WHERE s.environment_id=$1
 ) SELECT EXISTS(SELECT 1 FROM ancestors WHERE parent_session_id=$3)`, env, target, caller).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrDenied
	}
	return nil
}
