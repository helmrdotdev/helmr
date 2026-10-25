package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ExpireImageWaiter fails only never-dispatched work. Computer storage and
// the executor's physical lifetime remain owned by their separate reconcilers.
func ExpireImageWaiter(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, env, computerID uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		var spec *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT image_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computerID).Scan(&spec); err != nil {
			return err
		}
		if spec == nil {
			return nil
		}
		if err := computer.LockImageSpec(ctx, tx, env, *spec); err != nil {
			return err
		}
		read := func() ([]uuid.UUID, error) {
			rows, err := tx.Query(ctx, `WITH roots AS (SELECT DISTINCT root_session_id AS id FROM sessions WHERE environment_id=$1 AND computer_id=$2)
    SELECT s.id FROM sessions s JOIN roots r ON r.id=s.root_session_id WHERE s.environment_id=$1 ORDER BY s.id`, env, computerID)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		}
		before, err := read()
		if err != nil {
			return err
		}
		if _, err = LockSessionsAndComputers(ctx, tx, env, before, []uuid.UUID{computerID}); err != nil {
			return err
		}
		after, err := read()
		if err != nil {
			return err
		}
		if !slices.Equal(before, after) {
			return ErrNotReady
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at<=clock_timestamp()
   FROM computers WHERE environment_id=$1 AND id=$2`, env, computerID).Scan(&due); err != nil {
			return err
		}
		if !due {
			return nil
		}
		var dispatched bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND computer_id=$2 AND started_at IS NOT NULL)`, env, computerID).Scan(&dispatched); err != nil {
			return err
		}
		if dispatched {
			return ErrConflict
		}
		if err = computer.MarkPreparationFailed(ctx, tx, env, computerID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp()
   WHERE environment_id=$1 AND computer_id=$2 AND status='queued' RETURNING id,session_id`, env, computerID)
		if err != nil {
			return err
		}
		type failed struct{ turn, session uuid.UUID }
		failedTurns, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (failed, error) {
			var f failed
			err := row.Scan(&f.turn, &f.session)
			return f, err
		})
		if err != nil {
			return err
		}
		for _, turn := range failedTurns {
			if err = auditServiceFact(ctx, tx, auditPolicy, "image-preparation", audit.Event{EnvironmentID: env, SessionID: turn.session, TurnID: turn.turn, Action: "turn.failed", Outcome: "failed", TargetKind: "turn", TargetID: turn.turn, Details: audit.FailureDetails{Reason: "expired"}}); err != nil {
				return err
			}
			if err = eventData(ctx, tx, env, turn.session, turn.turn, "turn.failed", []byte(`{"code":"preparation_deadline"}`)); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE sessions SET status='closing' WHERE environment_id=$1 AND computer_id=$2 AND status='open'`, env, computerID); err != nil {
			return err
		}
		if err = notifySessionsChanged(ctx, tx, env, after...); err != nil {
			return err
		}
		return drainClosingSessions(ctx, auditPolicy, tx, env, after)
	}))
}
