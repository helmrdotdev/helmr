package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

// observedMessage is populated only by authenticated Slack reads/events. Metadata
// selects a candidate; actual author, destination and complete content must match
// the frozen mutation. An absent/mismatched message never permits a resend.
type observedMessage struct {
	StreamingState string          `json:"streaming_state"`
	Channel        string          `json:"channel"`
	User           string          `json:"user"`
	AppID          string          `json:"app_id"`
	Timestamp      string          `json:"ts"`
	Thread         string          `json:"thread_ts"`
	Text           string          `json:"text"`
	Blocks         json.RawMessage `json:"blocks"`
	Metadata       json.RawMessage `json:"metadata"`
}

// confirmMessage is an adapter-internal evidence consumer. It does not accept a
// user-submitted receipt, perform HTTP, authorize work, or advance any core state.
func confirmMessage(ctx context.Context, pool db.TxBeginner, installation uuid.UUID, message observedMessage) (bool, error) {
	if post := observedStreamPost(message.Blocks); post != uuid.Nil() {
		return confirmStreamMessage(ctx, pool, installation, post, message)
	}
	var metadata struct {
		EventType string `json:"event_type"`
		Payload   struct {
			PostID uuid.UUID `json:"post_id"`
		} `json:"event_payload"`
	}
	if json.Unmarshal(message.Metadata, &metadata) != nil || metadata.EventType != "helmr_post" || metadata.Payload.PostID == uuid.Nil() || !timestampPattern.MatchString(message.Timestamp) {
		return false, nil
	}
	confirmed := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var app, bot string
		err := tx.QueryRow(ctx, `SELECT app_id,bot_user_id FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, installation).Scan(&app, &bot)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if message.AppID != app || message.User != bot {
			return nil
		}
		var thread uuid.UUID
		var channel string
		err = tx.QueryRow(ctx, `SELECT p.thread_id,c.slack_channel_id FROM slack_posts p JOIN slack_threads sender ON sender.id=COALESCE(p.source_thread_id,p.thread_id)
 JOIN slack_channels c ON c.id=sender.channel_id WHERE p.id=$1 AND c.installation_id=$2`, metadata.Payload.PostID, installation).Scan(&thread, &channel)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if channel != message.Channel {
			return nil
		}
		var id uuid.UUID
		if err = tx.QueryRow(ctx, `SELECT id FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&id); err != nil {
			return err
		}
		var state string
		var disposed bool
		var frozen []byte
		var method *string
		if err = tx.QueryRow(ctx, `SELECT status,inflight_payload,inflight_method,delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, metadata.Payload.PostID).Scan(&state, &frozen, &method, &disposed); err != nil {
			return err
		}
		if (state != "uncertain" && !(state == "failed" && disposed)) || method == nil || (*method != "chat.postMessage" && *method != "chat.update") {
			return nil
		}
		var expected struct {
			Channel   string          `json:"channel"`
			Timestamp string          `json:"ts"`
			Thread    string          `json:"thread_ts"`
			Text      string          `json:"text"`
			Blocks    json.RawMessage `json:"blocks"`
			Metadata  json.RawMessage `json:"metadata"`
		}
		if err = json.Unmarshal(frozen, &expected); err != nil {
			return err
		}
		if expected.Channel != message.Channel || expected.Text != message.Text || !sameJSON(expected.Metadata, message.Metadata) || !sameMessageBlocks(expected.Blocks, message.Blocks) {
			return nil
		}
		if *method == "chat.update" {
			if expected.Timestamp != message.Timestamp {
				return nil
			}
		} else if expected.Thread != "" {
			if expected.Thread != message.Thread {
				return nil
			}
		} else if message.Thread != "" && message.Thread != message.Timestamp {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_posts SET status=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'failed' WHEN desired_revision=inflight_revision THEN 'posted' WHEN desired_revision<=suppressed_revision THEN 'suppressed' ELSE 'pending' END,
 confirmed_revision=inflight_revision,message_ts=$2,posted_at=COALESCE(posted_at,clock_timestamp()),error=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'delivery_abandoned' WHEN desired_revision>inflight_revision AND desired_revision<=suppressed_revision THEN 'publication_authority_unavailable' ELSE NULL END,next_attempt_at=clock_timestamp(),
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL
 WHERE id=$1`, metadata.Payload.PostID, message.Timestamp); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE slack_threads t SET thread_ts=$2 FROM slack_posts p WHERE p.id=$1 AND t.id=$3
 AND p.role='opening' AND t.front_session_id=p.session_id AND t.opening_publication_key=p.publication_key AND p.continuation_ordinal=0 AND t.thread_ts IS NULL`, metadata.Payload.PostID, message.Timestamp, thread); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}

func sameJSON(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	a, err := jsoncanon.Transform(a)
	if err != nil {
		return false
	}
	b, err = jsoncanon.Transform(b)
	return err == nil && bytes.Equal(a, b)
}

func sameMessageBlocks(expected, actual []byte) bool {
	if len(expected) == 0 {
		expected = []byte(`null`)
	}
	if len(actual) == 0 {
		actual = []byte(`null`)
	}
	var err error
	expected, err = jsoncanon.Transform(expected)
	if err != nil {
		return false
	}
	actual, err = jsoncanon.Transform(actual)
	if err != nil {
		return false
	}
	var want, got []map[string]json.RawMessage
	if json.Unmarshal(expected, &want) != nil || json.Unmarshal(actual, &got) != nil || len(want) != len(got) {
		return false
	}
	if len(want) == 0 {
		return true
	}
	for i := range want {
		// Slack generates an optional block identifier when the sender omits it.
		// Authored identifiers and every content/control field still match exactly.
		if want[i]["block_id"] == nil {
			delete(got[i], "block_id")
		}
	}
	a, err := json.Marshal(want)
	if err != nil {
		return false
	}
	b, err := json.Marshal(got)
	return err == nil && bytes.Equal(a, b)
}
