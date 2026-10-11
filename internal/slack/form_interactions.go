package slack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

func (h InteractionHandler) serveAnswerOpening(w http.ResponseWriter, r *http.Request, envelope interactionEnvelope) {
	action := envelope.Actions[0]
	claims, err := decodeControl(h.ControlKey, action.Value, time.Now())
	source, sourceErr := slackMessageTime(action.Timestamp)
	if err != nil || sourceErr != nil || claims.Action != "open_answer" || envelope.TriggerID == "" || len(envelope.TriggerID) > 4096 || source.After(time.Now().Add(5*time.Minute)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if h.Client == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	var view json.RawMessage
	var credential int64
	var rejected bool
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		question, ref, err := readFormQuestion(ctx, tx, h.AppID, envelope.Team.ID, envelope.User.ID, claims, source)
		if errors.Is(err, errFormIdentityUnlinked) {
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
			_, err = tx.Exec(ctx, `UPDATE slack_requests SET status='rejected',finished_at=clock_timestamp(),error='identity_unlinked' WHERE id=$1 AND status='received'`, receipt.ID)
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
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if rejected {
		w.WriteHeader(http.StatusOK)
		return
	}
	payload, _ := json.Marshal(map[string]any{"trigger_id": envelope.TriggerID, "view": view})
	result := h.Client.Call(ctx, claims.Target.Installation, credential, "views.open", payload)
	if result.Disposition != Acknowledged {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h InteractionHandler) serveFormSubmission(w http.ResponseWriter, r *http.Request, envelope interactionEnvelope) {
	claims, err := decodeControl(h.ControlKey, envelope.View.Metadata, time.Now())
	if (err != nil && !errors.Is(err, errControlExpired)) || !(envelope.View.CallbackID == "helmr.answer" && claims.Action == "answer") || envelope.User.IsBot || envelope.User.ID == "" || envelope.Team.ID == "" || claims.Actor != envelope.User.ID {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	source, err := slackMessageTime(claims.SourceTimestamp)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key, err := controlRequestKey(claims, envelope.User.ID, "")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// The first authenticated submission freezes the actual submitted state. Core
	// translation and live human authorization belong to the durable consumer, so
	// a denied submission cannot become new work after identity restoration.
	state := envelope.View.State.Values
	if state == nil {
		state = formState{}
	}
	payload, _ := json.Marshal(controlGesture{Control: claims, Form: state})
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
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
	if err != nil {
		switch {
		case errors.Is(err, errGestureConflict):
			w.WriteHeader(http.StatusConflict)
		case errors.Is(err, errControlInvalid), errors.Is(err, errGestureInvalid):
			w.WriteHeader(http.StatusBadRequest)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		return
	}
	w.WriteHeader(http.StatusOK)
}
