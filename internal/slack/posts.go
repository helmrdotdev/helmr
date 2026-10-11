package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// Both discovery and the locked claim use this dependency test. Discovery only
// omits known-blocked pending mutations; the claim remains authoritative.
const blockingPostPredecessor = `EXISTS(SELECT 1 FROM slack_posts prior
 WHERE prior.thread_source_id=p.thread_source_id AND prior.id<>p.id AND prior.delivery_disposed_at IS NULL AND (prior.status IN ('pending','sending','uncertain') OR (prior.presentation_path='stream' AND prior.stream_state='open')) AND
 ((prior.publication_key=p.publication_key AND prior.continuation_ordinal<p.continuation_ordinal)
 OR (p.role IN ('intermediate','response') AND prior.turn_id=p.turn_id AND prior.role='intermediate' AND prior.seq<p.seq)))`

type PostClaim struct {
	TeamID, ChannelID                           string
	PostID, ThreadID, InstallationID, AttemptID uuid.UUID
	CredentialRevision                          int64
	Method                                      string
	Epoch, Revision, StatusEpoch                int64
	Payload                                     []byte
	ExpiresAt, AuthorizedAt                     time.Time
}

// ClaimPost freezes one exact message mutation before issuing HTTP. Unknown
// content never becomes retryable merely because a worker lease expires. Stream
// start/stop atomically reserve the same thread lane used by native status repair.
func ClaimPost(ctx context.Context, pool db.TxBeginner, post uuid.UUID) (*PostClaim, error) {
	var claim *PostClaim
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var installation, channel, targetChannel, env, thread uuid.UUID
		var team, slackChannel string
		err := tx.QueryRow(ctx, `SELECT c.installation_id,c.id,p.environment_id,t.channel_id,p.thread_id,c.team_id,c.slack_channel_id
 FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id) JOIN slack_channels c ON c.id=sender.channel_id WHERE p.id=$1`, post).Scan(&installation, &channel, &env, &targetChannel, &thread, &team, &slackChannel)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		active, err := agent.LockSlackChannelsForDelivery(ctx, tx, env, []uuid.UUID{channel, targetChannel})
		if err != nil {
			return err
		}
		var credential int64
		var authorized time.Time
		var budget bool
		if err = tx.QueryRow(ctx, `SELECT credential_revision,authorized_at,delivery_next_at<=clock_timestamp() FROM slack_installations WHERE id=$1`, installation).Scan(&credential, &authorized, &budget); err != nil {
			return err
		}
		var root *string
		var opening *string
		var deleted, statusBusy, abandoned bool
		if err = tx.QueryRow(ctx, `SELECT thread_ts,opening_publication_key,deleted_at IS NOT NULL,(inflight_method IS NOT NULL AND claimed_until>clock_timestamp()) OR stream_status_repair,EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=slack_threads.id AND opening.role='opening' AND (opening.delivery_disposed_at IS NOT NULL OR (slack_threads.thread_ts IS NULL AND opening.status IN ('failed','suppressed')))) FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&root, &opening, &deleted, &statusBusy, &abandoned); err != nil {
			return err
		}
		var state, path, publication, role string
		var ordinal int
		var due, expired bool
		var revision int64
		var frozenRevision *int64
		var message *string
		var body, frozen []byte
		var method *string
		var stream streamSnapshot
		var streamText *string
		if err = tx.QueryRow(ctx, `SELECT role,status,presentation_path,publication_key,continuation_ordinal,next_attempt_at<=clock_timestamp(),
 COALESCE(claimed_until<=clock_timestamp(),false),desired_revision,message_ts,payload,inflight_payload,inflight_method,inflight_revision,stream_state,confirmed_stream_text,recipient_team_id,recipient_user_id,closed_at IS NOT NULL,suppressed_revision>=desired_revision,confirmed_revision,inflight_stream_text
 FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, post).Scan(&role, &state, &path, &publication, &ordinal, &due, &expired, &revision, &message, &body, &frozen, &method, &frozenRevision, &stream.state, &stream.confirmed, &stream.recipientTeam, &stream.recipientUser, &stream.closed, &stream.suppressed, &stream.confirmedRevision, &streamText); err != nil {
			return err
		}
		if path != "post" && path != "stream" {
			return nil
		}
		if state == "sending" && expired {
			if method != nil && *method == "chat.stopStream" {
				_, err = tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,error='stream_stop_unconfirmed',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, post)
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET status='uncertain',stream_state=CASE WHEN presentation_path='stream' THEN 'uncertain' ELSE stream_state END,claimed_until=NULL,error='worker_outcome_unknown' WHERE id=$1`, post)
			return err
		}
		if state != "pending" && !(path == "stream" && stream.state == "open" && stream.closed && (state == "posted" || state == "suppressed" || state == "failed")) {
			return nil
		}
		if deleted && !(path == "stream" && stream.state == "open" && stream.closed && message != nil) {
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET status='suppressed',error='root_deleted',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, post)
			return err
		}
		if !active || abandoned {
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET status='suppressed',error='publication_authority_unavailable',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, post)
			return err
		}
		if state == "failed" || deleted {
			stream.suppressed = true
		}
		isOpening := role == "opening" && opening != nil && *opening == publication && ordinal == 0
		if !due || !budget || (root == nil && !isOpening) {
			return nil
		}
		var blocked bool
		if err = tx.QueryRow(ctx, `SELECT `+blockingPostPredecessor+` FROM slack_posts p WHERE p.id=$1`, post).Scan(&blocked); err != nil {
			return err
		}
		if blocked {
			return nil
		}
		selected := "chat.postMessage"
		if len(frozen) == 0 && path == "stream" {
			var slackChannel string
			if err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM slack_channels WHERE id=$1`, channel).Scan(&slackChannel); err != nil {
				return err
			}
			mutation, err := prepareStreamMutation(ctx, tx, post, thread, env, slackChannel, root, message, body, revision, stream)
			if err != nil {
				return err
			}
			if mutation.method == "" {
				return nil
			}
			selected, frozen, revision = mutation.method, mutation.payload, mutation.revision
			streamText = &mutation.text
		} else if len(frozen) == 0 {
			var payload map[string]json.RawMessage
			if err = json.Unmarshal(body, &payload); err != nil || payload == nil {
				return errors.New("invalid frozen Slack message body")
			}
			var slackChannel string
			if err = tx.QueryRow(ctx, `SELECT slack_channel_id FROM slack_channels WHERE id=$1`, channel).Scan(&slackChannel); err != nil {
				return err
			}
			// Routing and reconciliation identifiers come from durable owners, never from
			// authored Content or appearance configuration.
			delete(payload, "ts")
			delete(payload, "thread_ts")
			payload["channel"], _ = json.Marshal(slackChannel)
			if message != nil {
				selected = "chat.update"
				payload["ts"], _ = json.Marshal(*message)
			} else if root != nil {
				payload["thread_ts"], _ = json.Marshal(*root)
			}
			digest := sha256.Sum256(body)
			payload["metadata"], _ = json.Marshal(map[string]any{"event_type": "helmr_post", "event_payload": map[string]any{"post_id": post.String(), "revision": revision, "content_digest": hex.EncodeToString(digest[:])}})
			frozen, err = json.Marshal(payload)
			if err != nil {
				return err
			}
		} else {
			if method == nil || (*method != "chat.postMessage" && *method != "chat.update" && *method != "chat.startStream" && *method != "chat.appendStream" && *method != "chat.stopStream") {
				return errors.New("invalid frozen Slack mutation")
			}
			selected = *method
			if frozenRevision == nil {
				return errors.New("missing frozen Slack revision")
			}
			revision = *frozenRevision
		}
		if statusAffectingStream(selected) && !deleted && statusBusy {
			return nil
		}
		digest := sha256.Sum256(frozen)
		c := &PostClaim{TeamID: team, ChannelID: slackChannel, PostID: post, ThreadID: thread, InstallationID: installation, CredentialRevision: credential, AuthorizedAt: authorized, AttemptID: uuid.NewV7(), Revision: revision, Method: selected, Payload: frozen}
		if err = tx.QueryRow(ctx, `UPDATE slack_posts SET status='sending',attempt_count=attempt_count+1,claim_epoch=claim_epoch+1,
 claimed_until=clock_timestamp()+interval '30 seconds',reconciliation_cursor='',reconciliation_pages=0,reconciliation_paused_at=NULL,inflight_method=$2,inflight_payload=$3,inflight_digest=$4,inflight_revision=$5,inflight_attempt_id=$6,inflight_stream_text=$7,error=CASE WHEN $2='chat.stopStream' THEN error ELSE NULL END
 WHERE id=$1 RETURNING claim_epoch,claimed_until`, post, selected, frozen, digest[:], revision, c.AttemptID, streamText).Scan(&c.Epoch, &c.ExpiresAt); err != nil {
			return err
		}
		if statusAffectingStream(selected) && !deleted {
			if err = claimStreamLane(ctx, tx, c, env); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_installations SET delivery_next_at=clock_timestamp()+interval '3 seconds' WHERE id=$1`, installation); err != nil {
			return err
		}
		claim = c
		return nil
	})
	return claim, err
}

// FinishPost records only this exact claim. The acknowledged revision may lag
// desired content. A known rate rejection retains the exact request for retry;
// every ambiguous outcome retains it for positive reconciliation, never replay.
func FinishPost(ctx context.Context, pool db.TxBeginner, claim PostClaim, result DeliveryResult) error {
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return finishPostClaim(ctx, tx, claim, result)
	})
}
func finishPostClaim(ctx context.Context, tx pgx.Tx, claim PostClaim, result DeliveryResult) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, claim.InstallationID).Scan(&id); err != nil {
		return err
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, claim.ThreadID).Scan(&id); err != nil {
		return err
	}
	var matches bool
	if err := tx.QueryRow(ctx, `SELECT status='sending' AND claim_epoch=$2 AND inflight_attempt_id=$3 FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, claim.PostID, claim.Epoch, claim.AttemptID).Scan(&matches); err != nil {
		return err
	}
	if !matches {
		return nil
	}
	if err := finishStreamLane(ctx, tx, claim, result); err != nil {
		return err
	}
	// A known-unsent completion after reauthorization/revocation cannot
	// restore old content into the new authorization window.
	if result.Disposition == NotIssued || result.Disposition == RateLimited || result.Disposition == Rejected {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT disconnected_at IS NULL AND authorization_lost_at IS NULL AND authorized_at=$2 FROM slack_installations WHERE id=$1`, claim.InstallationID, claim.AuthorizedAt).Scan(&active); err != nil {
			return err
		}
		if !active {
			_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='suppressed',claimed_until=NULL,error='publication_authority_unavailable',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, claim.PostID)
			return err
		}
	}
	if claim.Method == "chat.stopStream" && (result.Disposition == RateLimited || result.Disposition == NotIssued || (result.Disposition == Rejected && credentialRejection(result.Code))) {
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, claim.PostID); err != nil {
			return err
		}
	}
	switch result.Disposition {
	case Acknowledged:
		var submitted struct {
			Timestamp string `json:"ts"`
		}
		if err := json.Unmarshal(claim.Payload, &submitted); err != nil {
			return err
		}
		timestamp := result.Timestamp
		if claim.Method == "chat.update" || claim.Method == "chat.appendStream" || claim.Method == "chat.stopStream" {
			timestamp = submitted.Timestamp
		}
		if !timestampPattern.MatchString(timestamp) {
			_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='uncertain',claimed_until=NULL,error='missing_message_identity' WHERE id=$1`, claim.PostID)
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE slack_posts SET status=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'failed' WHEN inflight_method='chat.stopStream' AND error LIKE 'stream_content_rejected:%' THEN 'failed' WHEN desired_revision>inflight_revision AND desired_revision<=suppressed_revision THEN 'suppressed' WHEN presentation_path='stream' AND inflight_method<>'chat.stopStream' AND closed_at IS NOT NULL THEN 'pending' WHEN desired_revision=inflight_revision THEN 'posted' ELSE 'pending' END,
 stream_state=CASE WHEN presentation_path='stream' THEN CASE WHEN inflight_method='chat.stopStream' THEN 'stopped' ELSE 'open' END ELSE stream_state END,
 confirmed_stream_text=COALESCE(inflight_stream_text,confirmed_stream_text),inflight_stream_text=NULL,
 confirmed_revision=inflight_revision,message_ts=$2,posted_at=COALESCE(posted_at,clock_timestamp()),claimed_until=NULL,
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,error=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'delivery_abandoned' WHEN inflight_method='chat.stopStream' AND error LIKE 'stream_content_rejected:%' THEN error WHEN desired_revision>inflight_revision AND desired_revision<=suppressed_revision THEN 'publication_authority_unavailable' ELSE NULL END
 WHERE id=$1`, claim.PostID, timestamp)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE slack_threads t SET thread_ts=$2 FROM slack_posts p WHERE p.id=$1 AND t.id=$3
 AND p.role='opening' AND t.front_session_id=p.session_id AND t.opening_publication_key=p.publication_key AND p.continuation_ordinal=0 AND t.thread_ts IS NULL`, claim.PostID, timestamp, claim.ThreadID)
		return err
	case RateLimited:
		retry := result.RetryAfter
		if retry <= 0 {
			retry = time.Minute
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,next_attempt_at=clock_timestamp()+$2*interval '1 second',error='rate_limited' WHERE id=$1`, claim.PostID, retry.Seconds()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE slack_installations SET delivery_next_at=GREATEST(delivery_next_at,clock_timestamp()+$2*interval '1 second') WHERE id=$1`, claim.InstallationID, retry.Seconds())
		return err
	case NotIssued:
		code := result.Code
		if code == "" {
			code = "credential_unavailable"
		}
		_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,next_attempt_at=clock_timestamp()+interval '1 second',error=$2 WHERE id=$1`, claim.PostID, code)
		return err
	case Rejected:
		if credentialRejection(result.Code) {
			var retry bool
			if err := tx.QueryRow(ctx, `SELECT disconnected_at IS NULL AND authorization_lost_at IS NULL AND authorized_at=$2 AND (credential_revision<>$3 OR refresh_attempt_id IS NOT NULL) FROM slack_installations WHERE id=$1`, claim.InstallationID, claim.AuthorizedAt, claim.CredentialRevision).Scan(&retry); err != nil {
				return err
			}
			if retry {
				_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,next_attempt_at=clock_timestamp()+interval '1 second',error='credential_changed' WHERE id=$1`, claim.PostID)
				return err
			}
		}
		code := result.Code
		if code == "" {
			code = "delivery_rejected"
		}
		ended := (claim.Method == "chat.appendStream" || claim.Method == "chat.stopStream") && (code == "message_not_in_streaming_state" || code == "stopped_by_user" || code == "message_not_found")
		if claim.Method == "chat.stopStream" && ended && code != "message_not_found" {
			_, err := tx.Exec(ctx, `UPDATE slack_posts SET status=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'failed' WHEN error LIKE 'stream_content_rejected:%' THEN 'failed' WHEN desired_revision<=suppressed_revision THEN 'suppressed' ELSE 'posted' END,
 stream_state='stopped',claimed_until=NULL,error=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'delivery_abandoned' WHEN error LIKE 'stream_content_rejected:%' THEN error WHEN desired_revision<=suppressed_revision THEN 'publication_authority_unavailable' ELSE NULL END,
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, claim.PostID)
			return err
		}
		if claim.Method == "chat.appendStream" && !ended {
			code = "stream_content_rejected:" + code
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET status='failed',stream_state=CASE WHEN $3 THEN 'stopped' ELSE stream_state END,closed_at=COALESCE(closed_at,clock_timestamp()),claimed_until=NULL,error=$2,inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, claim.PostID, code, ended); err != nil {
			return err
		}
		switch result.Code {
		case "invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive", "missing_scope":
			return recordAuthorizationLoss(ctx, tx, claim.InstallationID, claim.CredentialRevision, claim.AuthorizedAt)
		}
		return nil
	default:
		if claim.Method == "chat.stopStream" {
			_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='pending',claimed_until=NULL,error='stream_stop_unconfirmed',inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL WHERE id=$1`, claim.PostID)
			return err
		}
		code := result.Code
		if code == "" {
			code = "delivery_outcome_unknown"
		}
		_, err := tx.Exec(ctx, `UPDATE slack_posts SET status='uncertain',stream_state=CASE WHEN presentation_path='stream' THEN 'uncertain' ELSE stream_state END,claimed_until=NULL,error=$2 WHERE id=$1`, claim.PostID, code)
		return err
	}
}

// postClaimAuthorized narrows the revocation race immediately before HTTP. It
// cannot fence an external request that has already started or retract its effect.
func postClaimAuthorized(ctx context.Context, pool db.TxBeginner, claim PostClaim) (bool, error) {
	var allowed bool
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_posts p
 JOIN slack_threads t ON t.id=p.thread_id AND (t.deleted_at IS NULL OR (p.inflight_method='chat.stopStream' AND p.message_ts IS NOT NULL AND p.closed_at IS NOT NULL))
 JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id)
 JOIN slack_channels c ON c.id=sender.channel_id JOIN slack_installations i ON i.id=c.installation_id
 JOIN agent_publications publication ON publication.id=c.publication_id JOIN slack_app_registrations registration ON registration.id=publication.slack_app_registration_id
 JOIN slack_channels target_channel ON target_channel.id=t.channel_id JOIN agent_publications target_publication ON target_publication.id=target_channel.publication_id
 JOIN slack_installations target_installation ON target_installation.id=target_channel.installation_id
 JOIN slack_app_registrations target_registration ON target_registration.id=target_publication.slack_app_registration_id
 WHERE p.id=$1 AND p.status='sending' AND p.claim_epoch=$2 AND p.inflight_attempt_id=$3 AND p.claimed_until>clock_timestamp()
 AND publication.revoked_at IS NULL AND registration.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL
 AND target_publication.revoked_at IS NULL AND target_registration.retired_at IS NULL AND target_installation.disconnected_at IS NULL AND target_installation.authorization_lost_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM slack_posts opening WHERE opening.thread_id=t.id AND opening.role='opening' AND (opening.delivery_disposed_at IS NOT NULL OR (t.thread_ts IS NULL AND opening.status IN ('failed','suppressed'))))
 AND i.id=$4 AND i.authorized_at=$5)`, claim.PostID, claim.Epoch, claim.AttemptID, claim.InstallationID, claim.AuthorizedAt).Scan(&allowed)
	})
	return allowed, err
}

func ReconcilePost(ctx context.Context, pool db.TxBeginner, client Caller, post uuid.UUID) (bool, error) {
	claim, err := ClaimPost(ctx, pool, post)
	if err != nil || claim == nil {
		return false, err
	}
	if err = client.VerifyChannel(ctx, claim.InstallationID, claim.CredentialRevision, claim.TeamID, claim.ChannelID); err != nil {
		result := DeliveryResult{Disposition: Rejected, Code: "channel_unavailable"}
		var failure *channelVerificationFailure
		if errors.As(err, &failure) {
			result = failure.result
		}
		return false, FinishPost(ctx, pool, *claim, result)
	}
	allowed, err := postClaimAuthorized(ctx, pool, *claim)
	if err != nil {
		return false, err
	}
	if !allowed {
		return false, FinishPost(ctx, pool, *claim, DeliveryResult{Disposition: Rejected, Code: "publication_authority_unavailable"})
	}
	requestCtx, cancel := context.WithDeadline(ctx, claim.ExpiresAt)
	result := client.Call(requestCtx, claim.InstallationID, claim.CredentialRevision, claim.Method, claim.Payload)
	cancel()
	return true, FinishPost(ctx, pool, *claim, result)
}
