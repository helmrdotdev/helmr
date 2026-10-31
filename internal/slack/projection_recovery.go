package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

// recoveryNotice renders one recovery event. A recovery gap is posted once per
// event; a storage pressure episode updates one post per hold.
func recoveryNotice(e projectionEvent) (string, string, error) {
	var value struct {
		HoldID     *string    `json:"holdId"`
		RestoredAt *time.Time `json:"restoredAt"`
		Affected   []string   `json:"affectedTurnIds"`
		State      string     `json:"state"`
	}
	if err := json.Unmarshal(e.data, &value); err != nil {
		return "", "", err
	}
	if e.kind == "session.recovery_gap" {
		point := "the initial work environment"
		if value.RestoredAt != nil {
			point = value.RestoredAt.UTC().Format("2006-01-02 15:04:05 UTC")
		}
		text := fmt.Sprintf("This Computer was restored to %s. File changes made after that point may be missing. No completed requests in this conversation were affected, and new requests can run.", point)
		if value.HoldID != nil {
			text = fmt.Sprintf("This Computer was restored to %s. File changes and native conversation history for %d completed requests in this conversation may be missing. Replies remain available. Mention me to continue, or choose Resume in Console. Completed requests will not run again.", point, len(value.Affected))
		}
		return fmt.Sprintf("recovery:%d", e.sequence), text, nil
	}
	if value.HoldID == nil || *value.HoldID == "" {
		return "", "", errors.New("storage pressure notice has no hold identity")
	}
	text := ""
	switch value.State {
	case "paused":
		text = "Work is waiting for storage space. Saving is still retrying. Unsaved changes may be lost if this environment becomes unavailable."
	case "stopped":
		text = "The request was stopped because storage stayed full. Local changes are retained while this environment remains available; recovery may omit changes not yet saved. New requests will wait until space is available."
	case "resolved":
		text = "Storage recovered. Queued work can continue. Stopped requests will not restart automatically."
	default:
		return "", "", errors.New("invalid storage pressure notice state")
	}
	return "pressure:" + *value.HoldID, text, nil
}

// Frozen in-flight mutations retain their original revision and reconcile normally.
func projectRecoveryNotice(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	key, text, err := recoveryNotice(e)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"text": text, "mrkdwn": false, "parse": "none", "blocks": []any{map[string]any{"type": "section", "text": plainText(text)}, consoleContext(o.consoleLink(), "View in Console")}})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,role,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,'lifecycle',$7,$8,'post',clock_timestamp() FROM slack_posts WHERE thread_source_id=$4
 ON CONFLICT(thread_source_id,post_key,continuation_ordinal) DO UPDATE SET payload=EXCLUDED.payload,payload_digest=EXCLUDED.payload_digest,desired_revision=slack_posts.desired_revision+1,status=CASE WHEN slack_posts.status='posted' THEN 'pending' ELSE slack_posts.status END,next_attempt_at=clock_timestamp()
 WHERE slack_posts.payload_digest<>EXCLUDED.payload_digest`, uuid.NewV7(), o.environment, o.session, o.participant, o.thread, key, body, digest[:])
	return err
}
