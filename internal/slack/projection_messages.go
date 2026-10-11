package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// Only an exact accepted Slack steering receipt gets public failure feedback.
// The core disposition remains terminal; projection never resubmits the message.
func projectMessageFailure(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	var value struct {
		ID       uuid.UUID `json:"message_id"`
		Delivery string    `json:"delivery"`
	}
	if err := json.Unmarshal(e.data, &value); err != nil {
		return err
	}
	if value.ID == uuid.Nil() || e.turn == nil {
		return errors.New("message rejection has no exact target")
	}
	var request uuid.UUID
	err := tx.QueryRow(ctx, `SELECT r.id FROM slack_requests r WHERE r.environment_id=$1 AND r.session_id=$2 AND r.turn_id=$3 AND r.message_id=$4 AND r.thread_source_id=$5 AND r.status='accepted' AND r.operation='send'`, o.environment, o.session, e.turn, value.ID, o.participant).Scan(&request)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	text := "Message not delivered. Send a new message when the Agent is available."
	if value.Delivery == "uncertain" {
		text = "Message delivery uncertain. Check the conversation in Console before sending it again."
	}
	body, err := json.Marshal(map[string]any{"text": text, "mrkdwn": false, "parse": "none", "blocks": []any{map[string]any{"type": "section", "text": plainText(text)}, map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "text": plainText("View in Console"), "url": o.consoleLink(), "action_id": "helmr.console_message"}}}}})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,publication_key,role,turn_id,request_id,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,'request_feedback',$7,$8,$9,$10,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,publication_key,continuation_ordinal) DO NOTHING`, uuid.NewV7(), o.environment, o.session, o.participant, o.thread, "message:"+value.ID.String(), e.turn, request, body, digest[:])
	return err
}
