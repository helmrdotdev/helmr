package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	sessionowner "github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

var ErrDeliveryUnavailable = errors.New("the Slack delivery is unavailable")

type DeliveryView struct {
	ID                uuid.UUID  `json:"id"`
	TurnID            *uuid.UUID `json:"turn_id"`
	Sequence          int64      `json:"sequence"`
	Role              string     `json:"role"`
	Continuation      int        `json:"continuation_ordinal"`
	Status            string     `json:"status"`
	Text              string     `json:"text"`
	PayloadExpiredAt  *time.Time `json:"payload_expired_at"`
	CreatedAt         time.Time  `json:"created_at"`
	DesiredRevision   int64      `json:"desired_revision"`
	ConfirmedRevision int64      `json:"confirmed_revision"`
	Method            *string    `json:"method"`
	AttemptID         *uuid.UUID `json:"attempt_id"`
	MessageTimestamp  *string    `json:"message_ts"`
	RootKnown         bool       `json:"root_known"`
	Opening           bool       `json:"opening"`
	StreamStatus      string     `json:"stream_status"`
	DisposedAt        *time.Time `json:"disposed_at"`
	Error             *string    `json:"error"`
}

// ListSessionDelivery exposes only diagnostics and authored fallback text, never
// signed controls, provider credentials or frozen transport request bodies.
func ListSessionDelivery(ctx context.Context, policy auditpolicy.Policy, pool db.TxBeginner, principal auth.Principal, environment, session uuid.UUID, after int64, limit int) ([]DeliveryView, error) {
	org := principal.OrgID
	result := []DeliveryView{}
	if after < 0 || limit < 1 || limit > 100 {
		return nil, ErrDeliveryUnavailable
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		err := sessionowner.AuthorizeSessionOperation(ctx, tx, sessionowner.UserCaller(principal), environment, session, auth.PermissionSessionsRead)
		if errors.Is(err, sessionowner.ErrDenied) {
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
 p.inflight_method,p.inflight_attempt_id,p.message_ts,t.thread_ts IS NOT NULL,
 p.role='opening' AND t.front_session_id=p.session_id AND t.opening_post_key=p.post_key AND p.continuation_ordinal=0,
 p.stream_status,p.delivery_disposed_at,p.error
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
			if err = rows.Scan(&v.ID, &v.TurnID, &v.Sequence, &v.Role, &v.Continuation, &v.Status, &raw, &v.PayloadExpiredAt, &v.CreatedAt, &v.DesiredRevision, &v.ConfirmedRevision, &v.Method, &v.AttemptID, &v.MessageTimestamp, &v.RootKnown, &v.Opening, &v.StreamStatus, &v.DisposedAt, &v.Error); err != nil {
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
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return sessionowner.RecordContentRead(ctx, policy, tx, sessionowner.UserCaller(principal), environment, session)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
