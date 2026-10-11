package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type StatusClaim struct {
	TeamID, ChannelID                   string
	ThreadID, InstallationID, AttemptID uuid.UUID
	CredentialRevision                  int64
	Epoch, Revision                     int64
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
		var team, slackChannel string
		err := tx.QueryRow(ctx, `SELECT c.id,c.environment_id,c.installation_id,c.team_id,c.slack_channel_id FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id WHERE t.id=$1`, thread).Scan(&channel, &env, &installation, &team, &slackChannel)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		active, err := agent.LockSlackChannelsForDelivery(ctx, tx, env, []uuid.UUID{channel})
		if err != nil {
			return err
		}
		var credential int64
		var authorized time.Time
		var budgetReady bool
		if err = tx.QueryRow(ctx, `SELECT credential_revision,authorized_at,delivery_next_at<=clock_timestamp() FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, installation).Scan(&credential, &authorized, &budgetReady); err != nil {
			return err
		}
		if !active {
			_, err = tx.Exec(ctx, `UPDATE slack_threads SET delivery_error='route_unavailable',status_confirmation='unknown' WHERE id=$1 AND deleted_at IS NULL`, thread)
			return err
		}
		var ts *string
		var oldStatus string
		var revision, confirmed int64
		var due, refresh, deleted bool
		if err = tx.QueryRow(ctx, `SELECT thread_ts,desired_status,desired_revision,confirmed_revision,next_attempt_at<=clock_timestamp() AND (claimed_until IS NULL OR claimed_until<=clock_timestamp()),refresh_at<=clock_timestamp() OR status_confirmation='unknown',deleted_at IS NOT NULL OR EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=slack_threads.id AND opening.role='opening' AND opening.delivery_disposed_at IS NOT NULL)
 FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&ts, &oldStatus, &revision, &confirmed, &due, &refresh, &deleted); err != nil {
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
			revision++
			if _, err = tx.Exec(ctx, `UPDATE slack_threads SET desired_status=$2,desired_revision=$3 WHERE id=$1`, thread, status, revision); err != nil {
				return err
			}
		}
		if !due || !budgetReady || (revision == confirmed && !refresh) {
			return nil
		}
		payload, err := json.Marshal(map[string]string{"channel_id": slackChannel, "thread_ts": *ts, "status": status})
		if err != nil {
			return err
		}
		digest := sha256.Sum256(payload)
		c := &StatusClaim{TeamID: team, ChannelID: slackChannel, ThreadID: thread, InstallationID: installation, CredentialRevision: credential, AuthorizedAt: authorized, AttemptID: uuid.NewV7(), Revision: revision, Payload: payload}
		if err = tx.QueryRow(ctx, `UPDATE slack_threads SET status_confirmation='unknown',inflight_method='agents.sessions.setStatus',inflight_payload=$2,inflight_digest=$3,inflight_revision=$4,inflight_attempt_id=$5,
 claim_epoch=claim_epoch+1,claimed_until=clock_timestamp()+interval '30 seconds'
 WHERE id=$1 RETURNING claim_epoch,claimed_until`, thread, payload, digest[:], revision, c.AttemptID).Scan(&c.Epoch, &c.ExpiresAt); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_installations SET delivery_next_at=clock_timestamp()+interval '3 seconds' WHERE id=$1`, installation); err != nil {
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
 SELECT s.id,s.parent_session_id FROM slack_thread_sources p JOIN sessions s ON (s.environment_id,s.id)=(p.environment_id,p.session_id)
 WHERE p.thread_id=$1 AND s.environment_id=$2 AND s.status NOT IN ('closed','cancelled')
 ), ancestors AS (
 SELECT id AS participant,id,parent_session_id FROM visible
 UNION ALL SELECT a.participant,s.id,s.parent_session_id FROM ancestors a JOIN sessions s ON s.environment_id=$2 AND s.id=a.parent_session_id
 ) SELECT CASE WHEN
 EXISTS(SELECT 1 FROM turn_asks q JOIN visible v ON q.session_id=v.id WHERE q.environment_id=$2 AND q.status='pending')
 OR EXISTS(SELECT 1 FROM session_holds h JOIN ancestors a ON h.session_id=a.id WHERE h.environment_id=$2 AND h.released_at IS NULL AND (a.id=a.participant OR h.scope='subtree'))
 THEN 'suspended' WHEN EXISTS(SELECT 1 FROM turns t JOIN visible v ON t.session_id=v.id WHERE t.environment_id=$2 AND t.status IN ('queued','running','finalizing'))
 THEN 'processing' ELSE 'active' END`, thread, env).Scan(&status)
	return status, err
}

// FinishStatus cannot fence an issued HTTP request. A stale completion is ignored;
// ambiguity releases this content-free lane so later current assertions can repair
// delayed effects. Content/stream uncertainty belongs to the affected post instead.
func FinishStatus(ctx context.Context, pool db.TxBeginner, claim StatusClaim, result DeliveryResult) error {
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
		tag, err := tx.Exec(ctx, `UPDATE slack_threads SET confirmed_revision=CASE WHEN $4 THEN GREATEST(confirmed_revision,inflight_revision) ELSE confirmed_revision END,
 status_confirmation=CASE WHEN $4 THEN 'acknowledged' ELSE 'unknown' END,delivery_error=NULLIF($5,''),
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,claimed_until=NULL,
 stream_status_repair=CASE WHEN $4 THEN false ELSE stream_status_repair END,
 next_attempt_at=clock_timestamp()+$6*interval '1 second',refresh_at=clock_timestamp()+interval '30 seconds'
 WHERE id=$1 AND claim_epoch=$2 AND inflight_attempt_id=$3 AND deleted_at IS NULL`, claim.ThreadID, claim.Epoch, claim.AttemptID, ack, result.Code, retry.Seconds())
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if result.Disposition == RateLimited {
			if _, err = tx.Exec(ctx, `UPDATE slack_installations SET delivery_next_at=GREATEST(delivery_next_at,clock_timestamp()+$2*interval '1 second') WHERE id=$1`, installation, retry.Seconds()); err != nil {
				return err
			}
		}
		if result.Disposition == Rejected {
			switch result.Code {
			case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope":
				return recordAuthorizationLoss(ctx, tx, installation, claim.CredentialRevision, claim.AuthorizedAt)
			}
		}
		return nil
	})
}

type Caller interface {
	VerifyChannel(context.Context, uuid.UUID, int64, string, string) error
	Call(context.Context, uuid.UUID, int64, string, []byte) DeliveryResult
}

func ReconcileStatus(ctx context.Context, pool db.TxBeginner, client Caller, thread uuid.UUID) (bool, error) {
	claim, err := ClaimStatus(ctx, pool, thread)
	if err != nil || claim == nil {
		return false, err
	}
	if err = client.VerifyChannel(ctx, claim.InstallationID, claim.CredentialRevision, claim.TeamID, claim.ChannelID); err != nil {
		return false, FinishStatus(ctx, pool, *claim, DeliveryResult{Disposition: Rejected, Code: "channel_unavailable"})
	}
	allowed, err := statusClaimAuthorized(ctx, pool, *claim)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, FinishStatus(ctx, pool, *claim, DeliveryResult{Disposition: Rejected, Code: "publication_authority_unavailable"})
	}
	requestCtx, cancel := context.WithDeadline(ctx, claim.ExpiresAt)
	result := client.Call(requestCtx, claim.InstallationID, claim.CredentialRevision, "agents.sessions.setStatus", claim.Payload)
	cancel()
	return true, FinishStatus(ctx, pool, *claim, result)
}

// Recheck a status claim immediately before HTTP, including deletion that may
// have invalidated it after claim commit. Already-started HTTP cannot be fenced.
func statusClaimAuthorized(ctx context.Context, pool db.TxBeginner, claim StatusClaim) (bool, error) {
	var allowed bool
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_threads t JOIN slack_channels c ON c.id=t.channel_id
 JOIN slack_installations i ON i.id=c.installation_id JOIN environments e ON e.id=c.environment_id
 JOIN agent_publications pub ON pub.id=c.publication_id JOIN slack_app_registrations reg ON reg.id=pub.slack_app_registration_id
 WHERE t.id=$1 AND t.claim_epoch=$2 AND t.inflight_attempt_id=$3 AND t.claimed_until>clock_timestamp() AND t.deleted_at IS NULL
 AND pub.revoked_at IS NULL AND reg.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.organization_id=e.org_id
 AND NOT EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=t.id AND opening.role='opening' AND opening.delivery_disposed_at IS NOT NULL)
 AND i.id=$4 AND i.authorized_at=$5)`, claim.ThreadID, claim.Epoch, claim.AttemptID, claim.InstallationID, claim.AuthorizedAt).Scan(&allowed)
	})
	return allowed, err
}
