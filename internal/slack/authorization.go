package slack

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/jackc/pgx/v5"
)

// recordAuthorizationLoss runs with the installation locked by the delivery
// transaction. A stale failure must not revoke a newer authorization. Pending
// content is suppressed atomically; already-issued uncertain sends retain their
// frozen evidence and are never converted into retryable work.
func recordAuthorizationLoss(ctx context.Context, tx pgx.Tx, policy auditpolicy.Policy, actor audit.Actor, installation uuid.UUID, credential int64, authorized time.Time) error {
	tag, err := tx.Exec(ctx, `UPDATE slack_installations SET authorization_lost_at=clock_timestamp()
 WHERE id=$1 AND credential_revision=$2 AND authorized_at=$3 AND authorization_lost_at IS NULL AND disconnected_at IS NULL AND refresh_attempt_id IS NULL`, installation, credential, authorized)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if err = suppressInstallationPosts(ctx, tx, installation); err != nil {
		return err
	}
	var org uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT org_id FROM slack_installations WHERE id=$1`, installation).Scan(&org); err != nil {
		return err
	}
	return audit.Append(ctx, tx, policy, audit.Event{Actor: actor, OrgID: org, Action: "slack_installation.authorization_lost", TargetKind: "slack_installation", TargetID: installation})
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
 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id
 WHERE (p.thread_id=t.id OR p.source_thread_id=t.id) AND c.installation_id=$1 `, installation)
	if err != nil {
		return err
	}
	// Content admission and projection both acquire the installation gate first.
	// Closing existing publications and disposing the unread prefix here prevents
	// a later cumulative update from carrying bytes across reauthorization.
	// Repeat this at reauthorization to exclude content accepted during the outage.
	if err = slackroute.SkipInstallationProjection(ctx, tx, installation); err != nil {
		return err
	}
	return dbnotify.Notify(ctx, tx, "slack_delivery")
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
