package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// RunSessionHistoryRetention is joined by the process root. Disk and diagnostic
// retention have separate owners and are not changed by this policy.
func RunSessionHistoryRetention(ctx context.Context, database db.TxDB, log *slog.Logger) error {
	if database == nil || log == nil {
		return errors.New("session history retention requires database and logger")
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	var after sessionLifecyclePosition
	for {
		next, more, err := reconcileSessionHistory(ctx, database, after)
		after = next
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			log.ErrorContext(ctx, "Session history retention failed", "error", err)
		}
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Discovery is keyset bounded: an obligated or failing Session cannot exclude
// later owners. Each candidate is rechecked under the normal Session lock order.
func reconcileSessionHistory(ctx context.Context, database db.TxDB, after sessionLifecyclePosition) (sessionLifecyclePosition, bool, error) {
	scan, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := database.Query(scan, `SELECT environment_id,id FROM sessions WHERE status IN ('closed','cancelled') AND history_expired_at IS NULL AND (history_eligible_at IS NULL OR history_expires_at<=EXTRACT(epoch FROM clock_timestamp())) AND (environment_id,id)>($1,$2) ORDER BY environment_id,id LIMIT 100`, after.environment, after.session)
	if err != nil {
		return after, false, err
	}
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sessionLifecyclePosition, error) {
		var p sessionLifecyclePosition
		err := row.Scan(&p.environment, &p.session)
		return p, err
	})
	if err != nil {
		return after, false, err
	}
	var failures []error
	for _, p := range candidates {
		if ctx.Err() != nil {
			return after, false, ctx.Err()
		}
		after = p
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := retainSessionHistory(attempt, database, p.environment, p.session)
		cancel()
		if err != nil {
			failures = append(failures, err)
		}
	}
	more := len(candidates) == 100
	if !more {
		after = sessionLifecyclePosition{}
	}
	return after, more, errors.Join(failures...)
}

const historyReleased = `s.status IN ('closed','cancelled')
 AND NOT EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.fenced_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM turns t WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND t.status IN ('queued','running','finalizing'))
 AND NOT EXISTS(SELECT 1 FROM turn_messages m WHERE m.environment_id=s.environment_id AND m.session_id=s.id AND m.status IN ('admitted','started'))
 AND NOT EXISTS(SELECT 1 FROM turn_asks a WHERE a.environment_id=s.environment_id AND a.session_id=s.id AND a.status='pending')
 AND NOT EXISTS(SELECT 1 FROM computer_saves save JOIN turns t ON t.environment_id=save.environment_id AND t.id=save.turn_id WHERE t.environment_id=s.environment_id AND t.session_id=s.id AND save.status IN ('requested','captured'))
 AND NOT EXISTS(SELECT 1 FROM computer_checkpoint_members m JOIN computer_checkpoints k ON k.environment_id=m.environment_id AND k.id=m.checkpoint_id WHERE m.environment_id=s.environment_id AND m.session_id=s.id AND k.status NOT IN ('cancelled','lost') AND k.controls_reconciled_at IS NULL)
 AND NOT EXISTS(SELECT 1 FROM slack_thread_sources p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND p.projected_event_seq<s.next_event_seq-1)
 AND NOT EXISTS(SELECT 1 FROM sessions child JOIN slack_thread_sources src ON (src.environment_id,src.session_id)=(child.environment_id,child.id)
 JOIN slack_threads thread ON thread.id=src.thread_id
 WHERE child.environment_id=s.environment_id AND child.parent_session_id=s.id AND thread.front_session_id=s.id AND
 (child.status NOT IN ('closed','cancelled') OR src.projected_event_seq<child.next_event_seq-1 OR
 EXISTS(SELECT 1 FROM session_processes p WHERE p.environment_id=child.environment_id AND p.session_id=child.id AND p.fenced_at IS NULL)))
 AND NOT EXISTS(SELECT 1 FROM slack_posts p WHERE p.environment_id=s.environment_id AND p.session_id=s.id AND
 (p.status IN ('pending','sending','uncertain') OR (p.inflight_attempt_id IS NOT NULL AND NOT (p.delivery_disposed_at IS NOT NULL AND p.presentation_path='post')) OR p.stream_state IN ('open','uncertain') OR (p.status='posted' AND p.desired_revision<>p.confirmed_revision)))`

func retainSessionHistory(ctx context.Context, database db.TxBeginner, environment, session uuid.UUID) error {
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if _, err := lockSession(ctx, tx, environment, session); err != nil {
			return err
		}
		// Projection must advance its participant cursor and freeze post intents in one
		// transaction. These two read predicates therefore observe either an unread
		// source or its outstanding delivery, never a gap between those obligations.
		// Owned sources also update the front Session's activity surface, so their
		// final physical-stop event must be projected before that surface expires.
		// The clock is established once. Subsequent reads and policy edits cannot
		// postpone or shorten the snapshot selected at Session admission.
		if _, err := tx.Exec(ctx, `WITH eligible AS (SELECT clock_timestamp() AS at FROM sessions s WHERE environment_id=$1 AND id=$2 AND history_eligible_at IS NULL AND `+historyReleased+`)
   UPDATE sessions s SET history_eligible_at=e.at,history_expires_at=CASE WHEN history_retention_mode='duration' THEN EXTRACT(epoch FROM e.at)+history_retention_seconds END FROM eligible e WHERE s.environment_id=$1 AND s.id=$2`, environment, session); err != nil {
			return err
		}
		var expired bool
		if err := tx.QueryRow(ctx, `SELECT COALESCE(history_expired_at IS NULL AND history_expires_at<=EXTRACT(epoch FROM clock_timestamp()) AND `+historyReleased+`,false) FROM sessions s WHERE environment_id=$1 AND id=$2`, environment, session).Scan(&expired); err != nil {
			return err
		}
		if !expired {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE turns SET input=NULL,result=NULL,error_message=NULL,response=NULL,response_expired_at=CASE WHEN response_id IS NOT NULL THEN COALESCE(response_expired_at,clock_timestamp()) END,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE turn_messages SET message=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE turn_asks SET question=NULL,answer=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE session_events SET data=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
			return err
		}
		// Slack mirrors share this Session's selected content-retention boundary.
		// Delivery obligations above must be settled first; keep publication and
		// request identities/digests so expired content never becomes fresh work.
		if _, err := tx.Exec(ctx, `UPDATE slack_requests SET payload=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2 AND status='accepted'`, environment, session); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET payload=NULL,payload_expired_at=COALESCE(payload_expired_at,clock_timestamp()),confirmed_stream_text='',
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL,closed_at=COALESCE(closed_at,clock_timestamp()) WHERE environment_id=$1 AND session_id=$2`, environment, session); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sessions SET history_expired_at=clock_timestamp() WHERE environment_id=$1 AND id=$2`, environment, session)
		return err
	})
}
