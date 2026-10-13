package session

import (
	"context"
	"uuid"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const historyReleased = `s.status IN ('closed','cancelled')
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status IN ('queued','running','finalizing'))
 AND NOT EXISTS(SELECT 1 FROM turn_messages m WHERE m.environment_id=s.environment_id AND m.session_id=s.id AND m.status IN ('admitted','started'))
 AND NOT EXISTS(SELECT 1 FROM turn_asks a WHERE a.environment_id=s.environment_id AND a.session_id=s.id AND a.status='pending')
 AND NOT EXISTS(SELECT 1 FROM computer_saves save JOIN turns t ON t.environment_id=save.environment_id AND t.id=save.turn_id WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND save.status IN ('requested','captured'))
 AND NOT EXISTS(SELECT 1 FROM checkpoint_processes m JOIN checkpoints k ON k.environment_id=m.environment_id AND k.id=m.checkpoint_id WHERE m.environment_id=s.environment_id AND m.session_id=s.id AND k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM slack_thread_sources p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.projected_event_seq<s.next_event_seq-1)
 AND NOT EXISTS(SELECT 1 FROM sessions child JOIN slack_thread_sources src ON (src.environment_id,src.session_id)=(child.environment_id,child.id)
 JOIN slack_threads thread ON thread.id=src.thread_id
 WHERE child.environment_id=s.environment_id AND child.parent_session_id=s.id AND thread.front_session_id=s.id AND
 (child.status NOT IN ('closed','cancelled') OR src.projected_event_seq<child.next_event_seq-1 OR
 EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=child.environment_id AND p.session_id=child.id AND p.fenced_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM slack_posts p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND
 (p.status IN ('pending','sending','uncertain') OR (p.inflight_attempt_id IS NOT NULL AND NOT (p.delivery_disposed_at IS NOT NULL AND p.presentation_path='post')) OR p.stream_state IN ('open','uncertain') OR (p.status='posted' AND p.desired_revision<>p.confirmed_revision)))`

// RetainHistory locks the Session, establishes its retention clock once its
// obligations are settled, and expires its content when due. It reports whether
// content expired. The caller must clear Slack copies in the same transaction.
func RetainHistory(ctx context.Context, tx pgx.Tx, environment, session uuid.UUID) (bool, error) {
	if _, err := lockSession(ctx, tx, environment, session); err != nil {
		return false, err
	}
	// A shared Project lock serializes cleanup with policy updates. Updates never
	// lock Sessions, so the lock order cannot form a cycle with cleanup.
	var seconds pgtype.Int8
	if err := tx.QueryRow(ctx, `SELECT p.history_retention_seconds FROM projects p JOIN environments e ON e.project_id=p.id WHERE e.id=$1 FOR SHARE OF p`, environment).Scan(&seconds); err != nil {
		return false, err
	}
	// Projection must advance its participant cursor and freeze post intents in one
	// transaction. These two read predicates therefore observe either an unread
	// source or its outstanding delivery, never a gap between those obligations.
	// Owned sources also update the front Session's activity surface, so their
	// final physical-stop event must be projected before that surface expires.
	// The clock is established once. Subsequent reads and policy edits cannot
	// reset the eligibility clock. The current Project policy governs expiry.
	if _, err := tx.Exec(ctx, `WITH eligible AS (SELECT clock_timestamp() AS at FROM sessions s WHERE environment_id=$1 AND id=$2 AND history_eligible_at IS NULL AND `+historyReleased+`)
   UPDATE sessions s SET history_eligible_at=e.at FROM eligible e WHERE s.environment_id=$1 AND s.id=$2`, environment, session); err != nil {
		return false, err
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(history_expired_at IS NULL AND EXTRACT(epoch FROM history_eligible_at)+$3::numeric<=EXTRACT(epoch FROM clock_timestamp()) AND `+historyReleased+`,false) FROM sessions s WHERE environment_id=$1 AND id=$2`, environment, session, seconds).Scan(&expired); err != nil {
		return false, err
	}
	if !expired {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE turns SET input=NULL,result=NULL,error_message=NULL,response=NULL,response_expired_at=CASE WHEN response_id IS NOT NULL THEN COALESCE(response_expired_at,clock_timestamp()) END,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE turn_messages SET message=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE turn_asks SET question=NULL,answer=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE session_events SET data=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
		return false, err
	}
	_, err := tx.Exec(ctx, `UPDATE sessions SET history_expired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, environment, session)
	return err == nil, err
}
