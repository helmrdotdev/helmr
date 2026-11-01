package slack

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/jackc/pgx/v5"
)

// settleUnknownDelivery ends the bounded check of one uncertain mutation. An
// unconfirmed opening whose thread has no root is sent once more when the check
// found no match or used its whole budget; replies queued behind it then post
// under whichever root is confirmed. Every other outcome disposes. Both attempts
// carry the same metadata, so either root message may be recognized and at most
// one duplicate opening remains in the channel.
func settleUnknownDelivery(ctx context.Context, tx pgx.Tx, post uuid.UUID, reason string, replaceable bool) error {
	if replaceable {
		resent, err := resendUnknownOpening(ctx, tx, post, reason)
		if err != nil || resent {
			return err
		}
	}
	return disposeUnknownDelivery(ctx, tx, post, reason)
}

// resendUnknownOpening returns an unconfirmed opening to pending under the thread
// gate. The thread records the resend, so a second unconfirmed opening disposes.
func resendUnknownOpening(ctx context.Context, tx pgx.Tx, post uuid.UUID, reason string) (bool, error) {
	var thread uuid.UUID
	err := tx.QueryRow(ctx, `SELECT thread_id FROM slack_posts WHERE id=$1 AND role='opening'`, post).Scan(&thread)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var eligible bool
	if err = tx.QueryRow(ctx, `SELECT thread_ts IS NULL AND opening_resent_at IS NULL FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&eligible); err != nil || !eligible {
		return false, err
	}
	tag, err := tx.Exec(ctx, `UPDATE slack_posts p SET status='pending',claimed_until=NULL,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL,next_attempt_at=clock_timestamp(),error=$2
 FROM slack_threads t WHERE p.id=$1 AND t.id=p.thread_id AND p.role='opening' AND p.continuation_ordinal=0 AND t.opening_post_key=p.post_key AND t.front_session_id=p.session_id
 AND p.status='uncertain' AND p.delivery_disposed_at IS NULL AND t.thread_ts IS NULL AND t.opening_resent_at IS NULL`, post, reason)
	if err != nil || tag.RowsAffected() != 1 {
		return false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE slack_threads SET opening_resent_at=clock_timestamp() WHERE id=$1`, thread); err != nil {
		return false, err
	}
	return true, dbnotify.Notify(ctx, tx, "slack_delivery")
}

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
