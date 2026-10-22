package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

// InteractionHandler verifies transport and the signed target, then commits a
// gesture receipt before ACK. It does not treat ACK as core operation success.
type InteractionHandler struct {
	AppOrigin     *url.URL
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
	State struct {
		Values formState `json:"values"`
	} `json:"state"`
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
	h.receiveInteractionPayload(r.Context(), []byte(values.Get("payload"))).writeHTTP(w)
}

func (h InteractionHandler) receiveInteractionPayload(parent context.Context, body []byte) receiptResult {
	ctx, cancel := context.WithTimeout(parent, receiptTimeout)
	defer cancel()
	payload, err := jsoncanon.Transform(body)
	if err != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	var envelope interactionEnvelope
	if json.Unmarshal(payload, &envelope) != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	if h.AppID == "" || envelope.AppID != h.AppID {
		return receiptResult{disposition: receiptUnauthorized}
	}
	if envelope.Type == "view_submission" && envelope.View.CallbackID == "helmr.answer" {
		return h.receiveFormSubmission(ctx, envelope)
	}
	if envelope.Type != "block_actions" {
		return receiptResult{disposition: receiptAccepted}
	}
	if len(envelope.Actions) != 1 || envelope.User.ID == "" || envelope.Team.ID == "" || envelope.User.IsBot {
		return receiptResult{disposition: receiptInvalid}
	}
	if envelope.Actions[0].ID == "helmr.console" {
		return receiptResult{disposition: receiptAccepted}
	}
	action := envelope.Actions[0]
	if action.Type == "button" && action.ID == "helmr.open_answer" {
		return h.receiveAnswerOpening(ctx, envelope)
	}
	if action.Type != "button" || (!strings.HasPrefix(action.ID, "helmr.choose.") && action.ID != "helmr.submit_choices" && !strings.HasPrefix(action.ID, "helmr.lifecycle.")) {
		return receiptResult{disposition: receiptAccepted}
	}
	claims, err := decodeControl(h.ControlKey, action.Value, time.Now())
	if (err != nil && !errors.Is(err, errControlExpired)) || (strings.HasPrefix(action.ID, "helmr.choose.") && claims.Action != "choose") || (action.ID == "helmr.submit_choices" && claims.Action != "submit_choices") || (strings.HasPrefix(action.ID, "helmr.lifecycle.") && (!lifecycleAction(claims.Action) || action.ID != "helmr.lifecycle."+claims.Action)) {
		return receiptResult{disposition: receiptInvalid}
	}
	key, err := controlRequestKey(claims, envelope.User.ID, action.Timestamp)
	if err != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	occurred, err := slackMessageTime(action.Timestamp)
	if err != nil || occurred.After(time.Now().Add(5*time.Minute)) {
		return receiptResult{disposition: receiptInvalid}
	}
	expires := occurred.Add(5 * time.Minute)
	if tokenExpiry := time.Unix(claims.ExpiresAt, 0); claims.ExpiresAt != 0 && tokenExpiry.Before(expires) {
		expires = tokenExpiry
	}
	gesture := controlGesture{Control: claims}
	if claims.Action == "submit_choices" {
		gesture.Form = formState{"choice": envelope.State.Values["choice"]}
	}
	normalized, _ := json.Marshal(gesture)
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
		if claims.Action == "choose" || claims.Action == "submit_choices" || lifecycleAction(claims.Action) {
			root := envelope.Message.ThreadTimestamp
			if root == "" {
				root = envelope.Message.Timestamp
			}
			var exact bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_threads WHERE id=$1 AND slack_channel_id=$2 AND thread_ts=$3)`, claims.Target.Thread, envelope.Channel.ID, root).Scan(&exact); err != nil {
				return err
			}
			if !exact {
				return errControlInvalid
			}
		}
		// Expired tokens can produce only a terminal rejection or retain an existing
		// receipt. ACK reveals no historical operation data and never admits core work.
		_, err = receiveGesture(ctx, tx, inboundGesture{Installation: installation, Key: key, Actor: envelope.User.ID, OccurredAt: occurred, ExpiresAt: expires, Payload: normalized})
		return err
	})
	return receiptOutcome(err)
}
