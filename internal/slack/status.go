package slack

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type StatusClaim struct {
	TeamID, ChannelID, PeerID           string
	ThreadID, InstallationID, AttemptID uuid.UUID
	CredentialRevision                  int64
	Status                              string
	Payload                             []byte
	ExpiresAt                           time.Time
	AuthorizedAt                        time.Time
}

// ClaimStatus serializes this app's native status lane. Recompute from core facts
// on every claim, including periodic refresh with no new event. No core owner is
// locked: status is an eventually convergent projection, not execution authority.
func ClaimStatus(ctx context.Context, pool db.TxBeginner, thread uuid.UUID) (*StatusClaim, error) {
	var claim *StatusClaim
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var channel, env, installation uuid.UUID
		var team, slackChannel, peer string
		err := tx.QueryRow(ctx, `SELECT c.id,c.environment_id,c.installation_id,c.team_id,c.slack_channel_id,CASE WHEN t.recipient_team_id=c.team_id THEN COALESCE(t.recipient_user_id,'') ELSE '' END FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id WHERE t.id=$1`, thread).Scan(&channel, &env, &installation, &team, &slackChannel, &peer)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		active, err := slackroute.LockChannelsForDelivery(ctx, tx, env, []uuid.UUID{channel})
		if err != nil {
			return err
		}
		recipient, err := directRecipientAuthorized(ctx, tx, thread)
		if err != nil {
			return err
		}
		active = active && recipient
		var credential int64
		var authorized time.Time
		if err = tx.QueryRow(ctx, `SELECT credential_revision,authorized_at FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, installation).Scan(&credential, &authorized); err != nil {
			return err
		}
		if !active {
			_, err = tx.Exec(ctx, `UPDATE slack_threads SET delivery_error='route_unavailable',status_confirmation='unknown' WHERE id=$1 AND deleted_at IS NULL`, thread)
			return err
		}
		var ts *string
		var oldStatus string
		var acknowledged *string
		var due, refresh, deleted bool
		if err = tx.QueryRow(ctx, `SELECT thread_ts,desired_status,last_acknowledged_status,next_attempt_at<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp()),refresh_at<=clock_timestamp() OR status_confirmation='unknown',deleted_at IS NOT NULL OR EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=slack_threads.id AND opening.role='opening' AND opening.delivery_disposed_at IS NOT NULL)
 FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&ts, &oldStatus, &acknowledged, &due, &refresh, &deleted); err != nil {
			return err
		}
		if ts == nil || deleted {
			return nil
		}
		status, err := aggregateStatus(ctx, tx, thread, env)
		if err != nil {
			return err
		}
		if oldStatus != status {
			if _, err = tx.Exec(ctx, `UPDATE slack_threads SET desired_status=$2 WHERE id=$1`, thread, status); err != nil {
				return err
			}
			if err = dbnotify.Notify(ctx, tx, "slack_delivery"); err != nil {
				return err
			}
		}
		if !due || (acknowledged != nil && *acknowledged == status && !refresh) {
			return nil
		}
		allowed, err := reserveDelivery(ctx, tx, installation, slackChannel, "agents.sessions.setStatus", false)
		if err != nil || !allowed {
			return err
		}
		payload, err := json.Marshal(map[string]string{"channel_id": slackChannel, "thread_ts": *ts, "status": status})
		if err != nil {
			return err
		}
		c := &StatusClaim{TeamID: team, ChannelID: slackChannel, PeerID: peer, ThreadID: thread, InstallationID: installation, CredentialRevision: credential, AuthorizedAt: authorized, AttemptID: uuid.NewV7(), Status: status, Payload: payload}
		if err = tx.QueryRow(ctx, `UPDATE slack_threads SET status_confirmation='unknown',inflight_method='agents.sessions.setStatus',inflight_attempt_id=$2,
 claimed_until=clock_timestamp()+interval '30 seconds'
 WHERE id=$1 RETURNING claimed_until`, thread, c.AttemptID).Scan(&c.ExpiresAt); err != nil {
			return err
		}
		claim = c
		return nil
	})
	return claim, err
}

func aggregateStatus(ctx context.Context, tx pgx.Tx, thread, env uuid.UUID) (string, error) {
	var status string
	err := tx.QueryRow(ctx, `WITH RECURSIVE visible AS (
 SELECT s.id,s.parent_session_id,s.status,s.authority_generation FROM slack_thread_sources p JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.thread_id=$1 AND s.environment_id=$2
 ), ancestors AS (
 SELECT id AS participant,id,parent_session_id FROM visible
 UNION ALL SELECT a.participant,s.id,s.parent_session_id FROM ancestors a JOIN sessions s ON s.environment_id=$2 AND s.id=a.parent_session_id
 ), held AS (
 SELECT DISTINCT a.participant AS id FROM session_holds h JOIN ancestors a ON h.session_id=a.id WHERE h.environment_id=$2 AND h.released_at IS NULL AND (a.id=a.participant OR h.kind='interrupt') AND EXISTS(SELECT 1 FROM visible v WHERE v.id=a.participant AND v.status NOT IN ('closed','cancelled'))
 ), waiting AS (
 SELECT id FROM held UNION SELECT q.session_id FROM turn_asks q JOIN visible v ON q.session_id=v.id WHERE q.environment_id=$2 AND q.status='pending'
 ) SELECT CASE WHEN
 EXISTS(SELECT 1 FROM session_processes p JOIN visible v ON p.session_id=v.id WHERE p.environment_id=$2 AND p.fenced_at IS NULL AND
 (p.status IN ('lost','stopping') OR p.stop_deadline_at IS NOT NULL OR p.control_error IS NOT NULL OR v.status IN ('closed','cancelled') OR
 (EXISTS(SELECT 1 FROM held h WHERE h.id=v.id) AND NOT COALESCE(p.control_kind='suspend' AND p.control_acknowledged_at IS NOT NULL AND p.control_generation=v.authority_generation AND p.control_attachment=p.attachment_sequence,false))))
 OR EXISTS(SELECT 1 FROM turns t JOIN visible v ON t.session_id=v.id WHERE t.environment_id=$2 AND t.status IN ('queued','running') AND NOT EXISTS(SELECT 1 FROM waiting w WHERE w.id=v.id))
 THEN 'processing' WHEN EXISTS(SELECT 1 FROM waiting) THEN 'suspended'
 WHEN EXISTS(SELECT 1 FROM visible) AND NOT EXISTS(SELECT 1 FROM visible WHERE status NOT IN ('closed','cancelled')) THEN
 CASE WHEN EXISTS(SELECT 1 FROM slack_thread_sources src JOIN session_events e ON (e.environment_id,e.session_id)=(src.environment_id,src.session_id) WHERE src.thread_id=$1 AND e.seq>src.projected_event_seq)
 OR EXISTS(SELECT 1 FROM slack_thread_sources src JOIN session_processes process ON (process.environment_id,process.session_id)=(src.environment_id,src.session_id) WHERE src.thread_id=$1 AND GREATEST(process.control_acknowledged_at,process.fenced_at,process.failure_recorded_at)>COALESCE(src.projected_control_at,'-infinity'::timestamptz))
 OR EXISTS(SELECT 1 FROM slack_posts post WHERE post.thread_id=$1 AND post.role<>'controls' AND post.delivery_disposed_at IS NULL AND (post.status IN ('pending','sending','uncertain') OR post.stream_status IN ('open','uncertain')))
 THEN 'processing' ELSE 'closed' END ELSE 'active' END`, thread, env).Scan(&status)
	return status, err
}

// FinishStatus cannot fence an issued HTTP request. A stale completion is ignored;
// ambiguity releases this content-free lane so later current assertions can repair
// delayed effects. Content/stream uncertainty belongs to the affected post instead.
func FinishStatus(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, claim StatusClaim, result DeliveryResult) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var installation uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, claim.InstallationID).Scan(&installation); err != nil {
			return err
		}
		ack := result.Disposition == Acknowledged
		retry := time.Duration(0)
		if !ack {
			retry = 30 * time.Second
		}
		if result.Disposition == RateLimited && result.RetryAfter > 0 {
			retry = result.RetryAfter
		}
		tag, err := tx.Exec(ctx, `UPDATE slack_threads SET last_acknowledged_status=CASE WHEN $4 THEN $3 ELSE last_acknowledged_status END,
 status_confirmation=CASE WHEN $4 THEN 'acknowledged' ELSE 'unknown' END,delivery_error=NULLIF($5,''),
 inflight_method=NULL,inflight_attempt_id=NULL,claimed_until=NULL,
 stream_status_repair=CASE WHEN $4 THEN false ELSE stream_status_repair END,
 next_attempt_at=clock_timestamp()+$6*interval '1 second',refresh_at=clock_timestamp()+interval '30 seconds'
 WHERE id=$1 AND inflight_attempt_id=$2 AND deleted_at IS NULL`, claim.ThreadID, claim.AttemptID, claim.Status, ack, result.Code, retry.Seconds())
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if result.Disposition == RateLimited {
			method := "agents.sessions.setStatus"
			if result.RateLimitMethod != "" {
				method = result.RateLimitMethod
			}
			if err = recordMethodBackoff(ctx, tx, installation, method, retry); err != nil {
				return err
			}
		}
		if result.Disposition == Rejected {
			switch result.Code {
			case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope":
				return recordAuthorizationLoss(ctx, tx, policy, audit.Internal(audit.ControlPlane), installation, claim.CredentialRevision, claim.AuthorizedAt)
			}
		}
		return nil
	})
}

type Caller interface {
	VerifyConversation(context.Context, uuid.UUID, int64, string, string, string) error
	Call(context.Context, uuid.UUID, int64, string, []byte) DeliveryResult
}

func ReconcileStatus(ctx context.Context, pool db.TxBeginner, policy auditpolicy.Policy, client Caller, thread uuid.UUID) (bool, error) {
	claim, err := ClaimStatus(ctx, pool, thread)
	if err != nil || claim == nil {
		return false, err
	}
	if err = client.VerifyConversation(ctx, claim.InstallationID, claim.CredentialRevision, claim.TeamID, claim.ChannelID, claim.PeerID); err != nil {
		result := DeliveryResult{Disposition: Rejected, Code: "channel_unavailable"}
		if failure, ok := errors.AsType[*channelVerificationFailure](err); ok {
			result = failure.result
		}
		return false, FinishStatus(ctx, pool, policy, *claim, result)
	}
	allowed, err := statusClaimAuthorized(ctx, pool, *claim)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, FinishStatus(ctx, pool, policy, *claim, DeliveryResult{Disposition: Rejected, Code: "publication_authority_unavailable"})
	}
	requestCtx, cancel := context.WithDeadline(ctx, claim.ExpiresAt)
	result := client.Call(requestCtx, claim.InstallationID, claim.CredentialRevision, "agents.sessions.setStatus", claim.Payload)
	cancel()
	return true, FinishStatus(ctx, pool, policy, *claim, result)
}

// Recheck a status claim immediately before HTTP, including deletion that may
// have invalidated it after claim commit. Already-started HTTP cannot be fenced.
func statusClaimAuthorized(ctx context.Context, pool db.TxBeginner, claim StatusClaim) (bool, error) {
	var allowed bool
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		allowed, err = directRecipientAuthorized(ctx, tx, claim.ThreadID)
		if err != nil || !allowed {
			return err
		}
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id
 JOIN slack_installations i ON i.id=c.installation_id JOIN environments e ON e.id=c.environment_id
 JOIN slack_connections pub ON pub.id=c.connection_id JOIN slack_app_registrations reg ON reg.id=pub.slack_app_registration_id
 WHERE t.id=$1 AND t.inflight_attempt_id=$2 AND t.claimed_until>clock_timestamp() AND t.deleted_at IS NULL
 AND pub.revoked_at IS NULL AND reg.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.org_id=e.org_id
 AND NOT EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=t.id AND opening.role='opening' AND opening.delivery_disposed_at IS NOT NULL)
 AND i.id=$3 AND i.authorized_at=$4)`, claim.ThreadID, claim.AttemptID, claim.InstallationID, claim.AuthorizedAt).Scan(&allowed)
	})
	return allowed, err
}
