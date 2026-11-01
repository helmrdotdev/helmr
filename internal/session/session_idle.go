package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// The idle clock counts only a continuously quiescent interval and is kept for
// every open Session; idle close and automatic hibernation both read it.
// Admission and control paths clear the clock under the same ownership locks.
// Internal saves and physical reconciliation defer closure without restarting
// that clock. Keep physical blockers aligned with desiredSessionControl and
// Computer capture/restore reconciliation; a parked ready checkpoint is already
// settled.
//
// A hold is not work: queued Turns that a hold blocks do not keep the Session
// open, and idle close cancels them like any close. An open child Session is
// not the parent's own work, so it does not stop the parent's clock, but it
// defers the parent's idle close until the child closes.
func reconcileSessionIdle(ctx context.Context, auditPolicy auditpolicy.Policy, database db.TxBeginner, env, session uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='1s'`); err != nil {
			return err
		}
		ids, _, err := lockControlTree(ctx, tx, env, session, Caller{})
		if err != nil {
			return err
		}
		var busy, closeDeferred bool
		err = tx.QueryRow(ctx, `SELECT
   EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='running')
   OR (EXISTS(SELECT 1 FROM turns WHERE environment_id=$1 AND session_id=$2 AND status='queued') AND EXISTS(SELECT 1 FROM sessions s WHERE s.environment_id=$1 AND s.id=$2 AND `+db.NoSessionHoldsSQL+`))
   OR EXISTS(SELECT 1 FROM turn_messages WHERE environment_id=$1 AND session_id=$2 AND status IN ('admitted','started'))
   OR EXISTS(SELECT 1 FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND status='pending')
   , EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND parent_session_id=$2 AND status<>'closed')
   OR EXISTS(SELECT 1 FROM computer_saves save JOIN sessions s ON s.environment_id=save.environment_id AND s.computer_id=save.computer_id WHERE s.environment_id=$1 AND s.id=$2 AND save.status IN ('requested','captured') AND (save.turn_id IS NULL OR EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=save.environment_id AND t.id=save.turn_id AND t.session_id=s.id)))
   OR EXISTS(SELECT 1 FROM session_processes p JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id) WHERE p.environment_id=$1 AND p.session_id=$2 AND p.fenced_at IS NULL AND (p.stop_deadline_at IS NOT NULL OR p.status<>'ready' OR p.control_acknowledged_at IS NULL OR p.control_error IS NOT NULL OR p.control_attachment<>p.attachment_sequence OR p.control_generation<>s.authority_generation OR p.control_kind IS DISTINCT FROM CASE WHEN `+db.SessionHeldSQL+` THEN 'suspend' ELSE 'resume' END))
   OR EXISTS(SELECT 1 FROM checkpoints cp JOIN checkpoint_processes m ON (m.environment_id,m.checkpoint_id)=(cp.environment_id,cp.id) WHERE cp.environment_id=$1 AND m.session_id=$2 AND (cp.status IN ('capturing','sealed','aborting','restoring') OR (cp.status='consumed' AND cp.controls_reconciled_at IS NULL)))`, env, session).Scan(&busy, &closeDeferred)
		if err != nil {
			return err
		}
		if busy {
			_, err = tx.Exec(ctx, `UPDATE sessions SET idle_since=NULL WHERE environment_id=$1 AND id=$2`, env, session)
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE sessions SET idle_since=COALESCE(idle_since,clock_timestamp()) WHERE environment_id=$1 AND id=$2 AND status='open'`, env, session); err != nil {
			return err
		}
		if closeDeferred {
			return nil
		}
		var due bool
		if err = tx.QueryRow(ctx, `SELECT s.status='open' AND d.close_after_idle_ms IS NOT NULL AND extract(epoch FROM (clock_timestamp()-s.idle_since))*1000>=d.close_after_idle_ms FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.agent_id,d.deployment_id)=(s.environment_id,s.agent_id,s.deployment_id) WHERE s.environment_id=$1 AND s.id=$2`, env, session).Scan(&due); err != nil || !due {
			return err
		}
		// session.closed with how=idle records the outcome.
		return beginClose(ctx, auditPolicy, tx, env, ids, "idle")
	})
}
