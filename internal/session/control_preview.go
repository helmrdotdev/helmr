package session

import (
	"context"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/jackc/pgx/v5"
)

// ComputerHibernatePreview counts the open Sessions on a Computer that a
// hibernate request would pause. It authorizes the same caller as hibernate.
func ComputerHibernatePreview(ctx context.Context, tx pgx.Tx, caller Caller, env, computerID uuid.UUID) (int64, error) {
	if _, err := authorizeComputerExecution(ctx, tx, caller, env, "hibernate"); err != nil {
		return 0, err
	}
	var exists bool
	var count int64
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers WHERE environment_id=$1 AND id=$2 AND deleted_at IS NULL),
 (SELECT count(*) FROM sessions WHERE environment_id=$1 AND computer_id=$2 AND status='open')`, env, computerID).Scan(&exists, &count); err != nil {
		return 0, err
	}
	if !exists {
		return 0, ErrDenied
	}
	return count, nil
}

// SessionClosePreview counts the Sessions that close with sessionID: the
// Session and the Sessions it owns that are not closed yet. It authorizes the
// same Sessions as close: the target and its owned Sessions that are not closed.
func SessionClosePreview(ctx context.Context, tx pgx.Tx, caller Caller, env, sessionID uuid.UUID) (int64, error) {
	if err := authorizeTarget(ctx, tx, caller, env, sessionID, auth.PermissionSessionsClose); err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `WITH RECURSIVE tree AS (SELECT id,status FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.id,s.status FROM sessions s JOIN tree t ON s.parent_session_id=t.id WHERE s.environment_id=$1)
 SELECT id FROM tree WHERE status<>'closed' ORDER BY id`, env, sessionID)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err := authorizeTarget(ctx, tx, caller, env, id, auth.PermissionSessionsClose); err != nil {
			return 0, err
		}
	}
	return int64(len(ids)), nil
}
