package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

// InteractionHandler verifies transport and the signed target, then commits a
// gesture receipt before ACK. It does not treat ACK as core operation success.
type InteractionHandler struct {
	PublicURL     *url.URL
	Client        Caller
	Database      db.TxBeginner
	AppID         string
	SigningSecret []byte
	ControlKey    []byte
}

type interactionEnvelope struct {
	TriggerID  string `json:"trigger_id"`
	CallbackID string `json:"callback_id"`
	ActionID   string `json:"action_id"`
	Value      string `json:"value"`
	Channel    struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		Timestamp       string `json:"ts"`
		ThreadTimestamp string `json:"thread_ts"`
	} `json:"message"`
	View struct {
		ID         string `json:"id"`
		Hash       string `json:"hash"`
		CallbackID string `json:"callback_id"`
		Metadata   string `json:"private_metadata"`
		State      struct {
			Values formState `json:"values"`
		} `json:"state"`
	} `json:"view"`
	Type  string `json:"type"`
	AppID string `json:"api_app_id"`
	Team  struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID    string `json:"id"`
		IsBot bool   `json:"is_bot"`
	} `json:"user"`
	Actions []struct {
		Type           string `json:"type"`
		ID             string `json:"action_id"`
		Value          string `json:"value"`
		Timestamp      string `json:"action_ts"`
		SelectedOption struct {
			Value string `json:"value"`
		} `json:"selected_option"`
	} `json:"actions"`
}

func (h InteractionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024*1024))
	if err != nil {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if h.AppID == "" || !VerifyRequest(h.SigningSecret, r.Header, body, time.Now()) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/x-www-form-urlencoded" {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 1 || len(values["payload"]) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	payload, err := jsoncanon.Transform([]byte(values.Get("payload")))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var envelope interactionEnvelope
	if json.Unmarshal(payload, &envelope) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if envelope.AppID != h.AppID {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	r = r.WithContext(ctx)
	if envelope.Type == "view_submission" && envelope.View.CallbackID == "helmr.answer" {
		h.serveFormSubmission(w, r, envelope)
		return
	}
	if envelope.Type != "block_actions" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if len(envelope.Actions) != 1 || envelope.User.ID == "" || envelope.Team.ID == "" || envelope.User.IsBot {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if envelope.Actions[0].ID == "helmr.console" {
		w.WriteHeader(http.StatusOK)
		return
	}
	action := envelope.Actions[0]
	if action.Type == "button" && action.ID == "helmr.open_answer" {
		h.serveAnswerOpening(w, r, envelope)
		return
	}
	if action.Type != "button" || action.ID != "helmr.stop" {
		w.WriteHeader(http.StatusOK)
		return
	}
	claims, err := decodeControl(h.ControlKey, action.Value, time.Now())
	if (err != nil && !errors.Is(err, errControlExpired)) || claims.Action != "stop" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key, err := controlRequestKey(claims, envelope.User.ID, action.Timestamp)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	occurred, err := slackMessageTime(action.Timestamp)
	if err != nil || occurred.After(time.Now().Add(5*time.Minute)) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	expires := occurred.Add(5 * time.Minute)
	if tokenExpiry := time.Unix(claims.ExpiresAt, 0); tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	normalized, _ := json.Marshal(controlGesture{Control: claims})
	err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		var installation uuid.UUID
		var bot string
		err := tx.QueryRow(ctx, `SELECT id,bot_user_id FROM slack_installations WHERE id=$1 AND app_id=$2 AND team_id=$3 FOR SHARE`, claims.Target.Installation, h.AppID, envelope.Team.ID).Scan(&installation, &bot)
		if errors.Is(err, pgx.ErrNoRows) {
			return errControlInvalid
		}
		if err != nil {
			return err
		}
		if envelope.User.ID == bot {
			return errControlInvalid
		}
		// Expired tokens can produce only a terminal rejection or retain an existing
		// receipt. ACK reveals no historical operation data and never admits core work.
		_, err = receiveGesture(ctx, tx, inboundGesture{Installation: installation, Key: key, Actor: envelope.User.ID, OccurredAt: occurred, ExpiresAt: expires, Payload: normalized})
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
