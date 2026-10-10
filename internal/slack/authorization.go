package slack

import (
	"context"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// recordAuthorizationLoss runs with the installation locked by the delivery
// transaction. A stale failure must not revoke a newer authorization. Pending
// content is suppressed atomically; already-issued uncertain sends retain their
// frozen evidence and are never converted into retryable work.
func recordAuthorizationLoss(ctx context.Context, tx pgx.Tx, installation uuid.UUID, credential int64, authorized time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp()
 WHERE id=$1 AND credential_revision=$2 AND authorized_at=$3 AND authorization_lost_at IS NULL AND disconnected_at IS NULL AND refresh_attempt_id IS NULL`, installation, credential, authorized)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return suppressInstallationPosts(ctx, tx, installation)
}

// Suppress desired-but-unsent content without losing an issued mutation's
// reconciliation evidence. Its later confirmation cannot requeue a revision
// that belonged to the retired authorization window.
func suppressInstallationPosts(ctx context.Context, tx pgx.Tx, installation uuid.UUID) error {
	_, err := tx.Exec(ctx, `UPDATE slack_posts p SET closed_at=COALESCE(p.closed_at,clock_timestamp()),
 suppressed_revision=CASE WHEN p.status IN ('pending','sending','uncertain') THEN p.desired_revision ELSE p.suppressed_revision END,
 status=CASE WHEN p.status='pending' THEN 'suppressed' ELSE p.status END,
 error=CASE WHEN p.status='pending' THEN 'installation_authorization_lost' ELSE p.error END,
 inflight_method=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_method END,
 inflight_payload=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_payload END,
 inflight_digest=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_digest END,
 inflight_revision=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_revision END,
 inflight_stream_text=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_stream_text END,
 inflight_attempt_id=CASE WHEN p.status='pending' THEN NULL ELSE p.inflight_attempt_id END
 FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id
 WHERE (p.thread_id=t.id OR p.source_thread_id=t.id) AND c.installation_id=$1 `, installation)
	if err != nil {
		return err
	}
	// Content admission and projection both acquire the installation gate first.
	// Closing existing publications and disposing the unread prefix here prevents
	// a later cumulative update from carrying bytes across reauthorization.
	// Repeat this at reauthorization to exclude content accepted during the outage.
	_, err = tx.Exec(ctx, `UPDATE slack_thread_sources p SET projected_event_seq=GREATEST(p.projected_event_seq,s.next_event_seq-1)
 FROM sessions s,slack_threads t,slack_channels c WHERE p.environment_id=s.environment_id AND p.session_id=s.id
 AND t.id=p.thread_id AND c.id=t.channel_id AND c.installation_id=$1`, installation)
	return err
}

// These responses establish that the rejected mutation did not execute. Missing
// scope is an authorization failure, not evidence that token rotation repairs it.
func credentialRejection(code string) bool {
	switch code {
	case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive":
		return true
	}
	return false
}
