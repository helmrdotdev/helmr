package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

const activityCardsPerPost = 10

type activityTask struct {
	ID             uuid.UUID
	Name           string
	Sequence       int64
	Status         string
	Held           bool
	ProcessStatus  *string
	ProcessPresent bool
	Suspended      bool
}

func (t activityTask) presentation() (string, string) {
	if t.Status == "completed" {
		return "complete", "Turn returned. See its authored activity for the outcome."
	}
	if t.ProcessStatus != nil && *t.ProcessStatus == "lost" {
		return "in_progress", "Status unavailable; cessation has not been confirmed."
	}
	if t.Status == "failed" {
		return "error", "Turn failed. View details in Console."
	}
	if t.Status == "interrupted" || t.Status == "cancelled" {
		if t.ProcessPresent && !t.Suspended {
			return "in_progress", "Stopping; waiting for execution to cease."
		}
		return "error", "Stopped."
	}
	if t.Held {
		if t.ProcessPresent && !t.Suspended {
			return "in_progress", "Stopping; waiting for execution to cease."
		}
		return "in_progress", "Waiting; the conversation is held."
	}
	switch t.Status {
	case "queued":
		return "in_progress", "Queued."
	case "running":
		return "in_progress", "Working."
	case "finalizing":
		return "in_progress", "Saving work."
	}
	return "in_progress", "Status unavailable."
}

// The activity surface is a projection of owned Turn attempts. It never reads
// machine results, native prompts or tool transcripts. Authored child Content
// keeps its own lossless continuation chain below this shared surface.
func projectActivity(ctx context.Context, tx pgx.Tx, o projectionOwner, authorized time.Time) error {
	rows, err := tx.Query(ctx, `SELECT t.id,a.name,t.seq,t.status,
 EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.released_at IS NULL AND (h.session_id=s.id OR (h.session_id=s.root_session_id AND h.scope='subtree'))),
 p.status,p.epoch IS NOT NULL,
 COALESCE(p.control_kind='suspend' AND p.control_acknowledged_at IS NOT NULL AND p.control_generation=s.authority_generation,false)
 FROM slack_thread_sources src JOIN sessions s ON (s.environment_id,s.id)=(src.environment_id,src.session_id)
 JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
 JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
 LEFT JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(s.environment_id,s.id,t.process_epoch) AND p.fenced_at IS NULL
 WHERE src.thread_id=$1 AND s.parent_session_id=$2 AND (t.created_at>=$3 OR t.status IN ('queued','running','finalizing'))
 ORDER BY t.created_at,t.id`, o.thread, o.front, authorized)
	if err != nil {
		return err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (activityTask, error) {
		var t activityTask
		err := row.Scan(&t.ID, &t.Name, &t.Sequence, &t.Status, &t.Held, &t.ProcessStatus, &t.ProcessPresent, &t.Suspended)
		return t, err
	})
	if err != nil || len(tasks) == 0 {
		return err
	}
	owner := o
	owner.session = o.front
	owner.participant = o.frontSource
	key := "activity:" + authorized.UTC().Format(time.RFC3339Nano)
	for offset := 0; offset < len(tasks); offset += activityCardsPerPost {
		end := min(offset+activityCardsPerPost, len(tasks))
		if err = projectActivityPage(ctx, tx, owner, key, offset/activityCardsPerPost, tasks[offset:end]); err != nil {
			return err
		}
	}
	return nil
}

func projectActivityPage(ctx context.Context, tx pgx.Tx, o projectionOwner, key string, ordinal int, tasks []activityTask) error {
	var prior uuid.UUID
	var previous []byte
	err := tx.QueryRow(ctx, `SELECT id,payload FROM slack_posts WHERE thread_source_id=$1 AND publication_key=$2 AND continuation_ordinal=$3 AND role='activity' FOR NO KEY UPDATE`, o.participant, key, ordinal).Scan(&prior, &previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var actions any
	if previous != nil {
		var body struct {
			Blocks []map[string]json.RawMessage `json:"blocks"`
		}
		if err = json.Unmarshal(previous, &body); err != nil {
			return err
		}
		for _, block := range body.Blocks {
			if string(block["type"]) == `"actions"` {
				actions = block
			}
		}
	}
	if actions == nil {
		claims := controlEnvelope{Nonce: uuid.NewV7(), Action: "stop", Target: controlTarget{Installation: o.installation, Thread: o.thread, Participant: o.participant, Environment: o.environment, Session: o.session}, ExpiresAt: time.Now().Add(30 * 24 * time.Hour).Unix()}
		token, err := encodeControl(o.config.ControlKey, claims)
		if err != nil {
			return err
		}
		actions = map[string]any{"type": "actions", "elements": []any{map[string]any{"type": "button", "text": plainText("View details"), "url": o.consoleLink(), "action_id": "helmr.console_activity"}, map[string]any{"type": "button", "text": plainText("Stop conversation"), "action_id": "helmr.stop", "value": token}}}
	}
	// Every message revision changes every task block ID; task IDs stay attached
	// to the original Turn. See Slack's task-card block update contract.
	type cardState struct {
		ID                     uuid.UUID
		Title, Status, Details string
	}
	states := make([]cardState, 0, len(tasks))
	closed := true
	text := "Delegated work"
	for _, task := range tasks {
		status, details := task.presentation()
		if status == "in_progress" {
			closed = false
		}
		title := fmt.Sprintf("%s · Turn %d", task.Name, task.Sequence)
		states = append(states, cardState{task.ID, title, status, details})
		text += "\n" + title + ": " + details
	}
	raw, _ := json.Marshal(states)
	hash := sha256.Sum256(raw)
	revision := hex.EncodeToString(hash[:12])
	blocks := []any{map[string]any{"type": "context", "elements": []any{plainText("Delegated work")}}}
	for _, state := range states {
		blocks = append(blocks, map[string]any{"type": "task_card", "task_id": state.ID.String(), "block_id": fmt.Sprintf("activity:%s:%d:%s:%s", o.thread, ordinal, revision, state.ID), "title": state.Title, "status": state.Status, "details": richBlock{Type: "rich_text", Elements: []richElement{{Type: "rich_text_section", Elements: []textElement{{Type: "text", Text: state.Details}}}}}})
	}
	blocks = append(blocks, actions)
	body, err := json.Marshal(map[string]any{"text": text, "mrkdwn": false, "parse": "none", "unfurl_links": false, "unfurl_media": false, "blocks": blocks})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(body)
	if prior != uuid.Nil() {
		_, err = tx.Exec(ctx, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,status=CASE WHEN status='posted' THEN 'pending' ELSE status END,closed_at=CASE WHEN $4 THEN COALESCE(closed_at,clock_timestamp()) ELSE NULL END
 WHERE id=$1 AND payload_digest<>$3 AND payload IS NOT NULL AND suppressed_revision=0 AND status NOT IN ('failed','suppressed')`, prior, body, digest[:], closed)
	} else {
		_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,publication_key,continuation_ordinal,role,payload,payload_digest,presentation_path,closed_at)
 SELECT $1,$2,$3,$4,$5,COALESCE(max(seq),0)+1,$6,$7,'activity',$8,$9,'post',CASE WHEN $10 THEN clock_timestamp() END FROM slack_posts WHERE thread_source_id=$4`, uuid.NewV7(), o.environment, o.session, o.participant, o.thread, key, ordinal, body, digest[:], closed)
	}
	return err
}
