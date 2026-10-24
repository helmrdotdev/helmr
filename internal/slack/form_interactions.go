package slack

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

func (h InteractionHandler) receiveAnswerOpening(ctx context.Context, envelope interactionEnvelope) receiptResult {
	action := envelope.Actions[0]
	claims, err := decodeControl(h.ControlKey, action.Value, time.Now())
	source, sourceErr := slackMessageTime(action.Timestamp)
	if err != nil || sourceErr != nil || claims.Action != "open_answer" || envelope.TriggerID == "" || len(envelope.TriggerID) > 4096 || source.After(time.Now().Add(5*time.Minute)) {
		return receiptResult{disposition: receiptInvalid}
	}
	if h.Client == nil {
		return receiptResult{disposition: receiptRetryable}
	}
	var view json.RawMessage
	var credential int64
	var rejected bool
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		question, ref, err := readFormQuestion(ctx, tx, h.AppID, envelope.Team.ID, envelope.User.ID, claims, source)
		if errors.Is(err, errFormIdentityUnlinked) || errors.Is(err, session.ErrDenied) {
			code := "permission_denied"
			if errors.Is(err, errFormIdentityUnlinked) {
				code = "identity_unlinked"
			}
			key, err := controlRequestKey(claims, envelope.User.ID, action.Timestamp)
			if err != nil {
				return err
			}
			payload, _ := json.Marshal(controlGesture{Control: claims})
			expires := source.Add(5 * time.Minute)
			if tokenExpiry := time.Unix(claims.ExpiresAt, 0); tokenExpiry.Before(expires) {
				expires = tokenExpiry
			}
			receipt, err := receiveGesture(ctx, tx, inboundGesture{Installation: claims.Target.Installation, Key: key, Actor: envelope.User.ID, OccurredAt: source, ExpiresAt: expires, Payload: payload})
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error=$2 WHERE id=$1 AND status='received'`, receipt.ID, code)
			rejected = true
			return err
		}
		if err != nil {
			return err
		}
		if question.Status != "pending" || question.Question == nil {
			return errControlInvalid
		}
		form := claims
		form.Nonce, form.Action, form.Actor, form.SourceTimestamp = uuid.NewV7(), "answer", envelope.User.ID, action.Timestamp
		if limit := source.Add(5 * time.Minute).Unix(); limit < form.ExpiresAt {
			form.ExpiresAt = limit
		}
		signed, err := encodeControl(h.ControlKey, form)
		if err != nil {
			return err
		}
		view, err = answerModal(question.Question, signed)
		credential = ref
		return err
	})
	if err != nil {
		return receiptOutcome(err)
	}
	if rejected {
		return receiptResult{disposition: receiptAccepted}
	}
	var channel string
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT slack_channel_id FROM slack_threads WHERE id=$1`, claims.Target.Thread).Scan(&channel)
	})
	if err != nil {
		return receiptOutcome(err)
	}
	if err = h.Client.VerifyConversation(ctx, claims.Target.Installation, credential, envelope.Team.ID, channel, envelope.User.ID); err != nil {
		return receiptOutcome(err)
	}
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		_, current, err := readFormQuestion(ctx, tx, h.AppID, envelope.Team.ID, envelope.User.ID, claims, source)
		if err != nil {
			return err
		}
		if current != credential {
			return errControlInvalid
		}
		return nil
	})
	if err != nil {
		return receiptOutcome(err)
	}
	payload, _ := json.Marshal(map[string]any{"trigger_id": envelope.TriggerID, "view": view})
	result := h.Client.Call(ctx, claims.Target.Installation, credential, "views.open", payload)
	if result.Disposition != Acknowledged {
		return receiptResult{disposition: receiptRetryable}
	}
	return receiptResult{disposition: receiptAccepted}
}

func (h InteractionHandler) receiveFormSubmission(ctx context.Context, envelope interactionEnvelope) receiptResult {
	claims, err := decodeControl(h.ControlKey, envelope.View.Metadata, time.Now())
	if (err != nil && !errors.Is(err, errControlExpired)) || !(envelope.View.CallbackID == "helmr.answer" && claims.Action == "answer") || envelope.User.IsBot || envelope.User.ID == "" || envelope.Team.ID == "" || claims.Actor != envelope.User.ID {
		return receiptResult{disposition: receiptInvalid}
	}
	source, err := slackMessageTime(claims.SourceTimestamp)
	if err != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	key, err := controlRequestKey(claims, envelope.User.ID, "")
	if err != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	// The first authenticated submission freezes the actual submitted state. Core
	// translation and live human authorization belong to the durable consumer, so
	// a denied submission cannot become new work after identity restoration.
	state := envelope.View.State.Values
	if state == nil {
		state = formState{}
	}
	payload, _ := json.Marshal(controlGesture{Control: claims, Form: state})
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		var bot string
		err := tx.QueryRow(ctx, `SELECT bot_user_id FROM slack_installations WHERE id=$1 AND app_id=$2 AND team_id=$3 FOR SHARE`, claims.Target.Installation, h.AppID, envelope.Team.ID).Scan(&bot)
		if errors.Is(err, pgx.ErrNoRows) {
			return errControlInvalid
		}
		if err != nil {
			return err
		}
		if bot == envelope.User.ID {
			return errControlInvalid
		}
		_, err = receiveGesture(ctx, tx, inboundGesture{Installation: claims.Target.Installation, Key: key, Actor: envelope.User.ID, OccurredAt: source, ExpiresAt: time.Unix(claims.ExpiresAt, 0), Payload: payload})
		return err
	})
	return receiptOutcome(err)
}
