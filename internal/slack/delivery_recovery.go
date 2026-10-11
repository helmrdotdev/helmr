package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

var ErrDeliveryUnavailable = errors.New("the Slack delivery is unavailable")
var ErrDeliveryChanged = errors.New("the Slack delivery changed; refresh before trying again")
var ErrDeliveryCheckUnavailable = errors.New("the original Slack connection is unavailable for delivery checks")

type DeliveryView struct {
	ID                  uuid.UUID  `json:"id"`
	TurnID              *uuid.UUID `json:"turn_id"`
	Sequence            int64      `json:"sequence"`
	Role                string     `json:"role"`
	Continuation        int        `json:"continuation_ordinal"`
	Status              string     `json:"status"`
	Text                string     `json:"text"`
	PayloadExpiredAt    *time.Time `json:"payload_expired_at"`
	CreatedAt           time.Time  `json:"created_at"`
	DesiredRevision     int64      `json:"desired_revision"`
	ConfirmedRevision   int64      `json:"confirmed_revision"`
	PublicationRevision int64      `json:"publication_revision"`
	Method              *string    `json:"method"`
	AttemptID           *uuid.UUID `json:"attempt_id"`
	MessageTimestamp    *string    `json:"message_ts"`
	RootKnown           bool       `json:"root_known"`
	Opening             bool       `json:"opening"`
	StreamState         string     `json:"stream_state"`
	CheckPausedAt       *time.Time `json:"check_paused_at"`
	DisposedAt          *time.Time `json:"disposed_at"`
	DisposedBy          *uuid.UUID `json:"disposed_by"`
	Error               *string    `json:"error"`
}

// ListSessionDelivery exposes only diagnostics and authored fallback text, never
// signed controls, provider credentials or frozen transport request bodies.
func ListSessionDelivery(ctx context.Context, pool db.TxBeginner, org, user, environment, session uuid.UUID, after int64, limit int) ([]DeliveryView, error) {
	result := []DeliveryView{}
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrDeliveryUnavailable
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var allowed bool
		err := tx.QueryRow(ctx, `SELECT m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer','viewer')
 FROM org_members m JOIN users u ON u.id=m.user_id WHERE m.org_id=$1 AND m.user_id=$2 FOR SHARE OF m,u`, org, user).Scan(&allowed)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !allowed {
			return ErrDeliveryUnavailable
		}
		if err != nil {
			return err
		}
		var exists bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN environments e ON e.id=s.environment_id WHERE e.org_id=$1 AND s.environment_id=$2 AND s.id=$3)`, org, environment, session).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrDeliveryUnavailable
		}
		rows, err := tx.Query(ctx, `SELECT p.id,p.turn_id,p.seq,p.role,p.continuation_ordinal,p.status,p.payload,p.payload_expired_at,p.created_at,
 p.desired_revision,p.confirmed_revision,
 (SELECT sum(v.desired_revision)::bigint FROM slack_posts v WHERE v.thread_source_id=p.thread_source_id AND v.publication_key=p.publication_key),
 p.inflight_method,p.inflight_attempt_id,p.message_ts,t.thread_ts IS NOT NULL,
 p.role='opening' AND t.front_session_id=p.session_id AND t.opening_publication_key=p.publication_key AND p.continuation_ordinal=0,
 p.stream_state,p.reconciliation_paused_at,p.delivery_disposed_at,p.delivery_disposed_by,p.error
 FROM slack_posts p JOIN slack_thread_sources s ON s.id=p.thread_source_id JOIN slack_threads t ON t.id=s.thread_id
 WHERE p.environment_id=$1 AND p.session_id=$2 AND p.seq>$3 AND (p.status IN ('uncertain','failed','suppressed') OR p.delivery_disposed_at IS NOT NULL)
 ORDER BY p.seq LIMIT $4`, environment, session, after, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v DeliveryView
			var raw []byte
			if err = rows.Scan(&v.ID, &v.TurnID, &v.Sequence, &v.Role, &v.Continuation, &v.Status, &raw, &v.PayloadExpiredAt, &v.CreatedAt, &v.DesiredRevision, &v.ConfirmedRevision, &v.PublicationRevision, &v.Method, &v.AttemptID, &v.MessageTimestamp, &v.RootKnown, &v.Opening, &v.StreamState, &v.CheckPausedAt, &v.DisposedAt, &v.DisposedBy, &v.Error); err != nil {
				return err
			}
			if raw != nil {
				var body struct {
					Text string `json:"text"`
				}
				if err = json.Unmarshal(raw, &body); err != nil {
					return err
				}
				v.Text = body.Text
			}
			result = append(result, v)
		}
		return rows.Err()
	})
	return result, err
}

type DeliveryRecovery struct {
	AttemptID           uuid.UUID `json:"attempt_id"`
	PublicationRevision int64     `json:"publication_revision"`
}

// RecoverDelivery requests read-only evidence or records a human's unsuccessful
// logical-publication disposition. Neither operation resends content, replaces a
// root, changes core work, or treats missing evidence as non-delivery.
func RecoverDelivery(ctx context.Context, pool db.TxBeginner, org, user, environment, session, post uuid.UUID, input DeliveryRecovery, action string) error {
	if input.AttemptID == uuid.Nil() || input.PublicationRevision < 1 || (action != "check" && action != "abandon") {
		return ErrDeliveryChanged
	}
	return db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockInstallationManager(ctx, tx, org, user); err != nil {
			return err
		}
		var installation, thread, participant uuid.UUID
		var publication string
		err := tx.QueryRow(ctx, `SELECT c.installation_id,p.thread_id,p.thread_source_id,p.publication_key
 FROM slack_posts p JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id) JOIN slack_channels c ON c.id=sender.channel_id
 JOIN environments e ON e.id=p.environment_id WHERE p.id=$1 AND p.environment_id=$2 AND p.session_id=$3 AND e.org_id=$4`, post, environment, session, org).Scan(&installation, &thread, &participant, &publication)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeliveryUnavailable
		}
		if err != nil {
			return err
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 AND organization_id=$2 FOR NO KEY UPDATE`, installation, org).Scan(&id); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&id); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_thread_sources WHERE id=$1 FOR NO KEY UPDATE`, participant).Scan(&id); err != nil {
			return err
		}
		var attempt *uuid.UUID
		var state string
		var disposed, paused bool
		if err = tx.QueryRow(ctx, `SELECT status,inflight_attempt_id,delivery_disposed_at IS NOT NULL,reconciliation_paused_at IS NOT NULL FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, post).Scan(&state, &attempt, &disposed, &paused); err != nil {
			return err
		}
		var revision int64
		var sending bool
		if err = tx.QueryRow(ctx, `SELECT sum(desired_revision)::bigint,bool_or(status='sending') FROM slack_posts WHERE thread_source_id=$1 AND publication_key=$2`, participant, publication).Scan(&revision, &sending); err != nil {
			return err
		}
		if attempt == nil || *attempt != input.AttemptID || revision != input.PublicationRevision || (state != "uncertain" && !(state == "failed" && disposed)) {
			return ErrDeliveryChanged
		}
		if action == "check" {
			if !paused {
				return ErrDeliveryChanged
			}
			var available bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id OR t.id=p.source_thread_id
 JOIN slack_channels c ON c.id=t.channel_id JOIN agent_publications pub ON pub.id=c.publication_id
 JOIN slack_app_registrations reg ON reg.id=pub.slack_app_registration_id JOIN slack_installations i ON i.id=c.installation_id
 WHERE p.id=$1 AND pub.revoked_at IS NULL AND reg.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL)`, post).Scan(&available); err != nil {
				return err
			}
			if !available {
				return ErrDeliveryCheckUnavailable
			}
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET reconciliation_cursor='',reconciliation_pages=0,reconciliation_paused_at=NULL,next_attempt_at=clock_timestamp() WHERE id=$1`, post)
			return err
		}
		if disposed {
			return nil
		}
		// A live mutation must first finish or become uncertain; do not overwrite its
		// claim or allow a late known-unsent completion to restore abandoned bytes.
		if sending {
			return ErrDeliveryChanged
		}
		// RuntimeOutput takes the same installation/thread gates before accepting
		// source content. Require its existing prefix to be projected and observed
		// before closing this publication; later output may start a fresh one.
		var unreadProgress bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM slack_posts p JOIN slack_thread_sources s ON s.id=p.thread_source_id
 JOIN session_events e ON e.environment_id=p.environment_id AND e.session_id=p.session_id AND e.turn_id=p.turn_id
 WHERE p.id=$1 AND p.role='intermediate' AND e.kind='turn.output' AND e.seq>s.projected_event_seq)`, post).Scan(&unreadProgress); err != nil {
			return err
		}
		if unreadProgress {
			return ErrDeliveryChanged
		}
		_, err = tx.Exec(ctx, `UPDATE slack_posts SET delivery_disposed_at=clock_timestamp(),delivery_disposed_by=$3,closed_at=COALESCE(closed_at,clock_timestamp()),
 suppressed_revision=desired_revision,reconciliation_paused_at=COALESCE(reconciliation_paused_at,clock_timestamp()),
 status=CASE WHEN status='uncertain' THEN 'failed' WHEN status='pending' THEN 'suppressed' ELSE status END,
 error=CASE WHEN status IN ('pending','uncertain') THEN 'delivery_abandoned' ELSE error END,
 inflight_method=CASE WHEN status='pending' THEN NULL ELSE inflight_method END,
 inflight_payload=CASE WHEN status='pending' THEN NULL ELSE inflight_payload END,
 inflight_digest=CASE WHEN status='pending' THEN NULL ELSE inflight_digest END,
 inflight_revision=CASE WHEN status='pending' THEN NULL ELSE inflight_revision END,
 inflight_attempt_id=CASE WHEN status='pending' THEN NULL ELSE inflight_attempt_id END,
 inflight_stream_text=CASE WHEN status='pending' THEN NULL ELSE inflight_stream_text END,
 next_attempt_at=clock_timestamp()
 WHERE thread_source_id=$1 AND publication_key=$2`, participant, publication, user)
		return err
	})
}
