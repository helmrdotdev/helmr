package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"uuid"
)

// FailQueuedExecutionTurns settles undispatched work under locked Computer
// membership after the execution provisioning deadline is rechecked.
func FailQueuedExecutionTurns(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, id uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp(),error_code='execution_deadline_exceeded',error_message='Worker execution capacity did not become available before the accepted deadline'
 WHERE environment_id=$1 AND computer_id=$2 AND status='queued' AND EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=turns.environment_id AND s.id=turns.session_id AND `+db.NoSessionHoldsSQL+`) RETURNING id,session_id`, env, id)
	if err != nil {
		return err
	}
	type turn struct{ id, session uuid.UUID }
	turns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (turn, error) {
		var t turn
		err := row.Scan(&t.id, &t.session)
		return t, err
	})
	if err != nil {
		return err
	}
	for _, t := range turns {
		if err = auditServiceFact(ctx, tx, auditPolicy, audit.ControlPlane, audit.Event{EnvironmentID: env, SessionID: t.session, TurnID: t.id, Action: "turn.failed", Outcome: "failed", TargetKind: "turn", TargetID: t.id, Details: audit.FailureDetails{Reason: "expired"}}); err != nil {
			return err
		}
		if err = eventData(ctx, tx, env, t.session, t.id, "turn.failed", []byte(`{"code":"execution_deadline_exceeded"}`)); err != nil {
			return err
		}
	}

	ids := make([]uuid.UUID, 0, len(turns))
	for _, t := range turns {
		ids = append(ids, t.session)
	}
	return notifySessionsChanged(ctx, tx, env, ids...)
}
