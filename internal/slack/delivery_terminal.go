package slack

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

// disposeUnknownDelivery settles local publication obligations without asserting
// whether Slack accepted the mutation. The caller holds the installation gate.
// Thread/source gates serialize the unread-prefix disposition with source admission,
// projection, mutation claims and positive evidence. No content is retried here.
func disposeUnknownDelivery(ctx context.Context, tx pgx.Tx, post uuid.UUID, reason string) error {
	var thread, source uuid.UUID
	var key string
	err := tx.QueryRow(ctx, `SELECT thread_id,thread_source_id,post_key FROM slack_posts WHERE id=$1`, post).Scan(&thread, &source, &key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var id uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&id); err != nil {
		return err
	}
	if err = tx.QueryRow(ctx, `SELECT id FROM slack_thread_sources WHERE id=$1 FOR NO KEY UPDATE`, source).Scan(&id); err != nil {
		return err
	}
	var eligible bool
	if err = tx.QueryRow(ctx, `SELECT status='uncertain' AND delivery_disposed_at IS NULL FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, post).Scan(&eligible); err != nil {
		return err
	}
	if !eligible {
		return nil
	}
	// All continuations belong to one logical publication. A live sibling claim
	// must complete or expire before it can be suppressed; retain its exact receipt.
	var sending bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE thread_source_id=$1 AND post_key=$2 AND status='sending')`, source, key).Scan(&sending); err != nil {
		return err
	}
	if sending {
		return nil
	}
	_, err = tx.Exec(ctx, `UPDATE slack_posts p SET delivery_disposed_at=clock_timestamp(),
 delivery_disposed_through_seq=s.next_event_seq-1,closed_at=COALESCE(p.closed_at,clock_timestamp()),
 suppressed_revision=p.desired_revision,
 status=CASE WHEN p.status='uncertain' THEN 'failed' WHEN p.status='pending' THEN 'suppressed' ELSE p.status END,
 error=CASE WHEN p.status IN ('pending','uncertain') THEN $3 ELSE p.error END,
 inflight_method=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_method END,
 inflight_payload=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_payload END,
 inflight_digest=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_digest END,
 inflight_revision=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_revision END,
 inflight_attempt_id=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_attempt_id END,
 inflight_stream_text=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_stream_text END,
 next_attempt_at=clock_timestamp()
 FROM sessions s WHERE p.thread_source_id=$1 AND p.post_key=$2 AND s.environment_id=p.environment_id AND s.id=p.session_id`, source, key, reason)
	if err != nil {
		return err
	}
	return dbnotify.Notify(ctx, tx, "slack_delivery")
}
