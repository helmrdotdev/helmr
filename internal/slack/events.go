package slack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/slackroute"
	"github.com/jackc/pgx/v5"
)

// EventHandler acknowledges only after a gesture receipt commits. Admission runs
// separately; neither channel contents nor transport authentication grant work.
type EventHandler struct {
	pendingRegistration bool
	Database            db.TxBeginner
	AppID               string
	SigningSecret       []byte
	Client              *WebClient
	Log                 *slog.Logger
}

type eventEnvelope struct {
	Type      string `json:"type"`
	AppID     string `json:"api_app_id"`
	TeamID    string `json:"team_id"`
	EventID   string `json:"event_id"`
	Challenge string `json:"challenge"`
	Event     struct {
		Type             string           `json:"type"`
		EventTimestamp   string           `json:"event_ts"`
		StreamingState   string           `json:"streaming_state"`
		Subtype          string           `json:"subtype"`
		User             string           `json:"user"`
		UserTeam         string           `json:"user_team"`
		SourceTeam       string           `json:"source_team"`
		BotID            string           `json:"bot_id"`
		AppID            string           `json:"app_id"`
		BotProfile       json.RawMessage  `json:"bot_profile"`
		Hidden           bool             `json:"hidden"`
		Edited           json.RawMessage  `json:"edited"`
		Channel          string           `json:"channel"`
		ChannelType      string           `json:"channel_type"`
		Tab              string           `json:"tab"`
		Timestamp        string           `json:"ts"`
		Thread           string           `json:"thread_ts"`
		Text             string           `json:"text"`
		Blocks           json.RawMessage  `json:"blocks"`
		Metadata         json.RawMessage  `json:"metadata"`
		Message          *observedMessage `json:"message"`
		DeletedTimestamp string           `json:"deleted_ts"`
		Files            []struct {
			ID string `json:"id"`
		} `json:"files"`
	} `json:"event"`
}

type messageGesture struct {
	Channel          string   `json:"channel"`
	Timestamp        string   `json:"message_ts"`
	Thread           string   `json:"thread_ts,omitempty"`
	Text             string   `json:"text"`
	UnsupportedFiles bool     `json:"unsupported_files,omitempty"`
	FileIDs          []string `json:"file_ids,omitempty"`
}

func (h EventHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	if (h.AppID == "" && !h.pendingRegistration) || !VerifyRequest(h.SigningSecret, r.Header, body, time.Now()) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	h.receiveEventPayload(r.Context(), body).writeHTTP(w)
}

func (h EventHandler) receiveEventPayload(parent context.Context, body []byte) receiptResult {
	ctx, cancel := context.WithTimeout(parent, receiptTimeout)
	defer cancel()
	body, err := jsoncanon.Transform(body)
	if err != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	var envelope eventEnvelope
	if json.Unmarshal(body, &envelope) != nil {
		return receiptResult{disposition: receiptInvalid}
	}
	if envelope.Type == "url_verification" {
		if envelope.Challenge == "" || len(envelope.Challenge) > 4096 || (h.AppID != "" && envelope.AppID != "" && envelope.AppID != h.AppID) {
			return receiptResult{disposition: receiptInvalid}
		}
		payload, _ := json.Marshal(map[string]string{"challenge": envelope.Challenge})
		return receiptResult{disposition: receiptAccepted, payload: payload}
	}
	if h.AppID == "" || envelope.AppID != h.AppID {
		return receiptResult{disposition: receiptUnauthorized}
	}
	if envelope.Type != "event_callback" {
		return receiptResult{disposition: receiptAccepted}
	}
	return receiptOutcome(h.receiveEvent(ctx, envelope))
}

func (h EventHandler) receiveEvent(ctx context.Context, envelope eventEnvelope) error {
	e := envelope.Event
	if e.Type == "app_home_opened" {
		return h.receiveAppHome(ctx, envelope)
	}
	if e.Type == "agent_session_stopped" {
		return h.receiveNativeStop(ctx, envelope)
	}
	if e.Type == "message" {
		if e.Subtype == "message_deleted" {
			return h.observeDeletion(ctx, envelope.TeamID, e.Channel, e.DeletedTimestamp)
		}
		if e.Subtype == "message_changed" && e.Message != nil {
			message := *e.Message
			message.Channel = e.Channel
			return h.observeEvent(ctx, envelope.TeamID, message)
		}
		if (e.Subtype == "" || e.Subtype == "bot_message") && e.AppID != "" {
			return h.observeEvent(ctx, envelope.TeamID, observedMessage{StreamingState: e.StreamingState, Channel: e.Channel, User: e.User, AppID: e.AppID, Timestamp: e.Timestamp, Thread: e.Thread, Text: e.Text, Blocks: e.Blocks, Metadata: e.Metadata})
		}
	}
	if e.Type != "message" && e.Type != "app_mention" {
		return nil
	}
	if e.User == "" || e.BotID != "" || e.AppID != "" || nonnullJSON(e.BotProfile) || nonnullJSON(e.Edited) || e.Hidden {
		return nil
	}
	switch e.Subtype {
	case "", "me_message", "file_share":
	default:
		return nil
	}
	if !validConversationID(e.Channel) || envelope.TeamID == "" || envelope.EventID == "" {
		return errGestureInvalid
	}
	occurred, err := slackMessageTime(e.Timestamp)
	if err != nil {
		return err
	}
	if e.Thread != "" && !timestampPattern.MatchString(e.Thread) {
		return errGestureInvalid
	}
	if occurred.After(time.Now().Add(5 * time.Minute)) {
		return errGestureInvalid
	}
	direct := slackroute.IsDirectMessage(e.Channel)
	if direct && (e.Type != "message" || e.ChannelType != "im" || e.UserTeam != "" && e.UserTeam != envelope.TeamID || e.SourceTeam != "" && e.SourceTeam != envelope.TeamID) {
		return nil
	}
	if !direct && e.ChannelType == "im" {
		return nil
	}
	if direct && e.Thread == e.Timestamp {
		e.Thread = ""
	}
	return db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		var installation uuid.UUID
		var bot string
		err := tx.QueryRow(ctx, `SELECT id,bot_user_id FROM slack_installations WHERE app_id=$1 AND team_id=$2 AND disconnected_at IS NULL FOR KEY SHARE`, h.AppID, envelope.TeamID).Scan(&installation, &bot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.User == bot {
			return nil
		}
		command := threadInputCommand(e.Text, bot)
		if command == "aside" {
			return nil
		}
		mentioned := stripRoutingMention(e.Text, bot) != e.Text
		var bound, muted, visible, open bool
		var unmuted *time.Time
		if e.Thread != "" {
			err = tx.QueryRow(ctx, `SELECT front.status='open',t.input_muted,t.input_unmuted_at,t.last_acknowledged_status IS NOT NULL OR EXISTS(SELECT 1 FROM slack_posts post WHERE post.thread_id=t.id AND post.message_ts IS NOT NULL) FROM slack_threads t JOIN sessions front ON (front.environment_id,front.id)=(t.environment_id,t.front_session_id) JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id
 WHERE c.installation_id=$1 AND c.slack_channel_id=$2 AND t.thread_ts=$3 AND t.deleted_at IS NULL`, installation, e.Channel, e.Thread).Scan(&open, &muted, &unmuted, &visible)
			bound = err == nil
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		// Channel roots require an actual mention. Once bound, human replies are
		// input regardless of whether Slack sends message or app_mention.
		if !direct && (!bound || !visible) && (!mentioned || e.Type == "message" && !bound) {
			return nil
		}
		if bound && open && command == "" && !mentioned && (muted || unmuted != nil && !occurred.After(*unmuted)) {
			return nil
		}
		gesture := messageGesture{Channel: e.Channel, Timestamp: e.Timestamp, Thread: e.Thread, Text: e.Text, UnsupportedFiles: len(e.Files) > 0 || e.Subtype == "file_share"}
		for _, file := range e.Files {
			gesture.FileIDs = append(gesture.FileIDs, file.ID)
		}
		payload, err := json.Marshal(gesture)
		if err != nil {
			return err
		}
		payload, err = jsoncanon.Transform(payload)
		if err != nil {
			return err
		}
		digest := gestureDigest(e.User, occurred, payload)
		// Only duplicate source notifications serialize; unrelated receipts must not
		// wait for core admission holding a shared installation lock.
		if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('slack-event:'||$1::uuid::text||':'||encode($2::bytea,'hex'),0))`, installation, digest[:]); err != nil {
			return err
		}
		var knownKey, sameGesture bool
		if err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM slack_requests WHERE installation_id=$1 AND request_key=$2),
 EXISTS(SELECT 1 FROM slack_requests WHERE installation_id=$1 AND request_digest=$3)`, installation, envelope.EventID, digest[:]).Scan(&knownKey, &sameGesture); err != nil {
			return err
		}
		// The source lock serializes competing notification types. Preserve
		// conflict checks on an existing key; only a new notification may be ignored.
		if !knownKey && sameGesture {
			return nil
		}
		// This bounds receipt admission waiting, not the lifetime or execution queue
		// of work after successful admission. Expired gestures need a new human action.
		_, err = receiveGesture(ctx, tx, inboundGesture{Installation: installation, Key: envelope.EventID, Actor: e.User, OccurredAt: occurred, ExpiresAt: occurred.Add(5 * time.Minute), Payload: payload})
		return err
	})
}

func nonnullJSON(value []byte) bool { return len(value) > 0 && string(value) != "null" }

func slackMessageTime(value string) (time.Time, error) {
	if !timestampPattern.MatchString(value) {
		return time.Time{}, errGestureInvalid
	}
	seconds, fraction, _ := strings.Cut(value, ".")
	sec, err := strconv.ParseInt(seconds, 10, 64)
	if err != nil || len(fraction) > 9 {
		return time.Time{}, errGestureInvalid
	}
	nanos, err := strconv.ParseInt(fraction+strings.Repeat("0", 9-len(fraction)), 10, 64)
	if err != nil {
		return time.Time{}, errGestureInvalid
	}
	return time.Unix(sec, nanos).UTC(), nil
}

// An authenticated delivery event may describe an older installation generation.
// Its exact post identifies that owner; selecting only the current installation
// would lose evidence after a disconnect/reconnect. This never admits human work.
func (h EventHandler) observeEvent(ctx context.Context, team string, message observedMessage) error {
	if message.AppID != h.AppID || team == "" || message.Channel == "" {
		return nil
	}
	var metadata struct {
		EventType string `json:"event_type"`
		Payload   struct {
			PostID uuid.UUID `json:"post_id"`
		} `json:"event_payload"`
	}
	post := observedStreamPost(message.Blocks)
	if post == uuid.Nil() {
		if json.Unmarshal(message.Metadata, &metadata) != nil || metadata.EventType != "helmr_post" || metadata.Payload.PostID == uuid.Nil() {
			return nil
		}
		post = metadata.Payload.PostID
	}
	var installation uuid.UUID
	err := db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT i.id FROM slack_posts p JOIN slack_threads t ON t.id=p.thread_id
 JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN slack_installations i ON i.id=c.installation_id
 WHERE p.id=$1 AND i.app_id=$2 AND i.team_id=$3 AND i.bot_user_id=$4 AND c.slack_channel_id=$5`, post, h.AppID, team, message.User, message.Channel).Scan(&installation)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var org uuid.UUID
	if err = db.RunTx(ctx, h.Database, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT org_id FROM slack_installations WHERE id=$1`, installation).Scan(&org)
	}); err != nil {
		return err
	}
	if matched, e := confirmOpeningMessage(ctx, h.Database, org, team, message); e != nil {
		return e
	} else if matched {
		return nil
	}
	_, err = confirmMessage(ctx, h.Database, installation, message)
	return err
}
