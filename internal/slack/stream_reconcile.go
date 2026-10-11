package slack

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// A public marker only selects the candidate. It never authenticates the sender
// or establishes that any part of a frozen mutation reached Slack.
func observedStreamPost(raw []byte) uuid.UUID {
	var blocks []struct {
		ID   string `json:"block_id"`
		Type string `json:"type"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return uuid.Nil()
	}
	var found uuid.UUID
	for _, b := range blocks {
		if b.Type != "context" || !strings.HasPrefix(b.ID, "helmr_stream_") {
			continue
		}
		id, err := uuid.Parse(strings.TrimPrefix(b.ID, "helmr_stream_"))
		if err != nil || id == uuid.Nil() || found != uuid.Nil() {
			return uuid.Nil()
		}
		found = id
	}
	return found
}

// Only lossless observed representations are accepted. Slack may translate
// Markdown into other block shapes; unrecognized shapes remain uncertain rather
// than using fallback text or a matching prefix as proof of complete publication.
func sameStreamContent(post uuid.UUID, body []byte, frozen string, actual []byte) bool {
	header, err := streamHeader(post, body)
	if err != nil {
		return false
	}
	var blocks []json.RawMessage
	if json.Unmarshal(actual, &blocks) != nil || len(blocks) < len(header) {
		return false
	}
	wantHeader, err := json.Marshal(header)
	if err != nil {
		return false
	}
	gotHeader, err := json.Marshal(blocks[:len(header)])
	if err != nil || !sameMessageBlocks(wantHeader, gotHeader) {
		return false
	}
	content := blocks[len(header):]
	var markdown strings.Builder
	allMarkdown := true
	for _, raw := range content {
		var b map[string]json.RawMessage
		if json.Unmarshal(raw, &b) != nil {
			return false
		}
		var kind, text string
		if json.Unmarshal(b["type"], &kind) != nil || kind != "markdown" {
			allMarkdown = false
			break
		}
		if json.Unmarshal(b["text"], &text) != nil {
			return false
		}
		for k := range b {
			if k != "type" && k != "text" && k != "block_id" {
				return false
			}
		}
		markdown.WriteString(text)
	}
	if allMarkdown {
		return markdown.String() == frozen
	}
	// Literal rich-text is re-serialized through the same escaping/fencing rules.
	// Reject links, mentions, styles, nested or unknown fields before conversion.
	for _, raw := range content {
		if !literalStreamBlock(raw) {
			return false
		}
	}
	raw, err := json.Marshal(map[string]any{"blocks": content})
	if err != nil {
		return false
	}
	text, err := streamContent(raw)
	return err == nil && text == frozen
}

func literalStreamBlock(raw []byte) bool {
	var b map[string]json.RawMessage
	if json.Unmarshal(raw, &b) != nil {
		return false
	}
	var kind string
	if json.Unmarshal(b["type"], &kind) != nil || kind != "rich_text" {
		return false
	}
	for k := range b {
		if k != "type" && k != "elements" && k != "block_id" {
			return false
		}
	}
	var sections []map[string]json.RawMessage
	if json.Unmarshal(b["elements"], &sections) != nil {
		return false
	}
	for _, section := range sections {
		if json.Unmarshal(section["type"], &kind) != nil || (kind != "rich_text_section" && kind != "rich_text_preformatted") {
			return false
		}
		for k := range section {
			if k != "type" && k != "elements" {
				return false
			}
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(section["elements"], &parts) != nil {
			return false
		}
		for _, p := range parts {
			if json.Unmarshal(p["type"], &kind) != nil || kind != "text" {
				return false
			}
			var text string
			if json.Unmarshal(p["text"], &text) != nil {
				return false
			}
			for k := range p {
				if k != "type" && k != "text" {
					return false
				}
			}
		}
	}
	return true
}

func confirmStreamMessage(ctx context.Context, pool db.TxBeginner, installation, post uuid.UUID, message observedMessage) (bool, error) {
	if !timestampPattern.MatchString(message.Timestamp) || (message.StreamingState != "in_progress" && message.StreamingState != "completed" && message.StreamingState != "errored") {
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
 JOIN slack_channels c ON c.id=sender.channel_id WHERE p.id=$1 AND c.installation_id=$2`, post, installation).Scan(&thread, &channel)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if message.Channel != channel {
			return nil
		}
		var root *string
		var statusEpoch int64
		if err = tx.QueryRow(ctx, `SELECT thread_ts,claim_epoch FROM slack_threads WHERE id=$1 FOR NO KEY UPDATE`, thread).Scan(&root, &statusEpoch); err != nil {
			return err
		}
		if root == nil || *root != message.Thread {
			return nil
		}
		var state, path string
		var disposed bool
		var attempt *uuid.UUID
		var method, timestamp, text *string
		var body []byte
		if err = tx.QueryRow(ctx, `SELECT status,presentation_path,inflight_method,message_ts,inflight_stream_text,payload,inflight_attempt_id,delivery_disposed_at IS NOT NULL FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, post).Scan(&state, &path, &method, &timestamp, &text, &body, &attempt, &disposed); err != nil {
			return err
		}
		if (state != "uncertain" && !(state == "failed" && disposed)) || attempt == nil || path != "stream" || method == nil || text == nil || (*method != "chat.startStream" && *method != "chat.appendStream") {
			return nil
		}
		if timestamp != nil && *timestamp != message.Timestamp {
			return nil
		}
		if *method == "chat.appendStream" && timestamp == nil {
			return nil
		}
		if !sameStreamContent(post, body, *text, message.Blocks) {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE slack_posts SET
 status=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'failed' WHEN $3='errored' OR ($3='completed' AND desired_revision>inflight_revision AND desired_revision>suppressed_revision) THEN 'failed'
 WHEN desired_revision>inflight_revision AND desired_revision<=suppressed_revision THEN 'suppressed'
 WHEN $3='in_progress' AND (closed_at IS NOT NULL OR desired_revision>inflight_revision) THEN 'pending' ELSE 'posted' END,
 error=CASE WHEN delivery_disposed_at IS NOT NULL THEN 'delivery_abandoned' WHEN $3='errored' THEN 'stream_errored' WHEN $3='completed' AND desired_revision>inflight_revision AND desired_revision>suppressed_revision THEN 'stream_ended_before_delivery' WHEN desired_revision>inflight_revision AND desired_revision<=suppressed_revision THEN 'publication_authority_unavailable' ELSE NULL END,
 stream_state=CASE WHEN $3='in_progress' THEN 'open' ELSE 'stopped' END,
 closed_at=CASE WHEN $3='in_progress' THEN closed_at ELSE COALESCE(closed_at,clock_timestamp()) END,
 confirmed_revision=inflight_revision,confirmed_stream_text=inflight_stream_text,message_ts=$2,posted_at=COALESCE(posted_at,clock_timestamp()),next_attempt_at=clock_timestamp(),
 inflight_method=NULL,inflight_payload=NULL,inflight_digest=NULL,inflight_revision=NULL,inflight_attempt_id=NULL,inflight_stream_text=NULL
 WHERE id=$1`, post, message.Timestamp, message.StreamingState)
		if err != nil {
			return err
		}
		// Exact positive evidence also settles a crashed start owner's duplicate
		// content in the thread status lane. A different participant's newer lane
		// is protected by the attempt identity; native status still needs repair.
		if err = finishStreamLane(ctx, tx, PostClaim{ThreadID: thread, StatusEpoch: statusEpoch, AttemptID: *attempt}, DeliveryResult{Disposition: Acknowledged}); err != nil {
			return err
		}
		confirmed = true
		return nil
	})
	return confirmed, err
}
