package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"
	"uuid"
)

const activityCardsPerPost = 10

type activityTask struct {
	ID, Session                                     uuid.UUID
	ParentTurn                                      *uuid.UUID
	CreatedAt                                       time.Time
	FrontCaller                                     bool
	Name                                            string
	Sequence                                        int64
	Status                                          string
	Held                                            bool
	ProcessStatus                                   *string
	ProcessPresent, Suspended, Stopping, StopTarget bool
}

func (t activityTask) presentation() (string, string) {
	if t.Status == "completed" {
		return "complete", "Execution completed. View the result in Console."
	}
	if t.ProcessStatus != nil && *t.ProcessStatus == "lost" {
		return "in_progress", "Status unavailable; cessation has not been confirmed."
	}
	if t.Stopping {
		return "in_progress", "Stopping; waiting for execution to cease."
	}
	if t.Status == "failed" {
		return "error", "Execution failed. View details in Console."
	}
	if t.Status == "interrupted" || t.Status == "cancelled" {
		// A healthy surviving process can acknowledge a turn-only interruption.
		// Failure/loss above never counts as acknowledgement; cancellation still
		// requires the whole process to cease.
		if t.Status == "interrupted" && !t.StopTarget {
			return "error", "Stopped."
		}
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
	}
	return "in_progress", "Status unavailable."
}

// Child facts decorate an existing front-authored work message. The persistent
// attachment fixes the reading position across questions, completion and retry.
// A child alone never opens a new Slack post or publishes its transcript.
func projectActivity(ctx context.Context, tx pgx.Tx, o projectionOwner, authorized time.Time) error {
	rows, err := tx.Query(ctx, `SELECT t.id,s.id,COALESCE(t.origin_turn_id,s.origin_turn_id),a.key,t.seq,t.status,
 EXISTS(SELECT 1 FROM session_holds h WHERE h.environment_id=s.environment_id AND h.released_at IS NULL AND (h.session_id=s.id OR (h.session_id=s.root_session_id AND h.scope='subtree'))),
 p.status,p.epoch IS NOT NULL,
 COALESCE(p.control_kind='suspend' AND p.control_acknowledged_at IS NOT NULL AND p.control_error IS NULL AND p.control_generation=s.authority_generation AND p.control_attachment=p.attachment_sequence,false),
 COALESCE((p.stop_deadline_at IS NOT NULL AND (p.stop_turn_id=t.id OR p.stop_turn_id IS NULL)) OR p.status='stopping',false),COALESCE(p.stop_turn_id=t.id,false),t.created_at,t.caller_kind='session' AND t.caller_id=$2
 FROM slack_thread_sources src JOIN sessions s ON (s.environment_id,s.id)=(src.environment_id,src.session_id)
 JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id) JOIN turns t ON (t.environment_id,t.session_id)=(s.environment_id,s.id)
 LEFT JOIN session_processes p ON (p.environment_id,p.session_id,p.epoch)=(s.environment_id,s.id,t.process_epoch) AND p.fenced_at IS NULL
 WHERE src.thread_id=$1 AND s.parent_session_id=$2 AND (t.created_at>=$3 OR t.status IN ('queued','running')) ORDER BY t.created_at,t.id`, o.thread, o.front, authorized)
	if err != nil {
		return err
	}
	tasks, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (activityTask, error) {
		var t activityTask
		err := row.Scan(&t.ID, &t.Session, &t.ParentTurn, &t.Name, &t.Sequence, &t.Status, &t.Held, &t.ProcessStatus, &t.ProcessPresent, &t.Suspended, &t.Stopping, &t.StopTarget, &t.CreatedAt, &t.FrontCaller)
		return t, err
	})
	if err != nil {
		return err
	}
	grouped := map[uuid.UUID][]activityTask{}
	omitted := map[uuid.UUID]int{}
	for _, task := range tasks {
		allowed, err := directRecipientCanReadSession(ctx, tx, o.thread, task.Session)
		if err != nil {
			return err
		}
		if !allowed {
			continue
		}
		var post uuid.UUID
		var hidden bool
		err = tx.QueryRow(ctx, `SELECT post_id,omitted FROM slack_work_items WHERE thread_id=$1 AND turn_id=$2`, o.thread, task.ID).Scan(&post, &hidden)
		if errors.Is(err, pgx.ErrNoRows) {
			if task.ParentTurn == nil {
				if !task.FrontCaller {
					continue
				}
				err = originlessActivityPost(ctx, tx, o, task).Scan(&post)
			} else {
				err = tx.QueryRow(ctx, `SELECT p.id FROM slack_posts p JOIN turns t ON (t.environment_id,t.id)=(p.environment_id,p.turn_id)
 WHERE p.thread_source_id=$1 AND p.turn_id=$2 AND p.role='intermediate' AND p.source_start_seq IS NOT NULL AND p.payload IS NOT NULL AND p.suppressed_revision=0 AND p.status NOT IN ('failed','suppressed')
 AND (p.closed_at IS NULL OR t.status IN ('completed','failed','interrupted','cancelled')) ORDER BY p.seq DESC LIMIT 1`, o.frontSource, task.ParentTurn).Scan(&post)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if err = tx.QueryRow(ctx, `SELECT count(*)>=$2 FROM slack_work_items WHERE post_id=$1 AND NOT omitted`, post, activityCardsPerPost).Scan(&hidden); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, `INSERT INTO slack_work_items(thread_id,environment_id,session_id,turn_id,post_id,omitted) VALUES($1,$2,$3,$4,$5,$6)`, o.thread, o.environment, task.Session, task.ID, post, hidden); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if hidden {
			omitted[post]++
		} else {
			grouped[post] = append(grouped[post], task)
		}
	}
	for post, tasks := range grouped {
		if err = projectActivityPost(ctx, tx, o, post, tasks, omitted[post]); err != nil {
			return err
		}
	}
	return nil
}

// Session-scoped callers do not claim an originating Turn. Attach their display
// facts to the front explanation that existed at admission, without inventing
// execution provenance. Projection timestamps cannot establish that ordering.
func originlessActivityPost(ctx context.Context, tx pgx.Tx, o projectionOwner, task activityTask) pgx.Row {
	return tx.QueryRow(ctx, `SELECT p.id FROM slack_posts p
 JOIN turns t ON (t.environment_id,t.id)=(p.environment_id,p.turn_id)
 JOIN session_events start ON (start.environment_id,start.session_id,start.seq)=(p.environment_id,p.session_id,p.source_start_seq)
 JOIN slack_thread_sources src ON src.id=p.thread_source_id
 WHERE p.thread_source_id=$1 AND p.role='intermediate' AND p.payload IS NOT NULL
 AND p.suppressed_revision=0 AND p.status NOT IN ('failed','suppressed')
 AND t.started_at<=$2 AND (t.processing_closed_at IS NULL OR t.processing_closed_at>$2) AND (t.terminal_at IS NULL OR t.terminal_at>$2)
 AND start.created_at<=$2
 AND NOT EXISTS(SELECT 1 FROM session_events pending WHERE pending.environment_id=p.environment_id AND pending.session_id=p.session_id AND pending.created_at<=$2 AND pending.seq>GREATEST(src.projected_event_seq,$3))
 AND NOT EXISTS(SELECT 1 FROM session_events boundary WHERE boundary.environment_id=p.environment_id AND boundary.session_id=p.session_id AND boundary.turn_id=p.turn_id AND boundary.kind='ask.created' AND boundary.seq>p.source_start_seq AND boundary.created_at<=$2)
 ORDER BY p.seq DESC LIMIT 1`, o.frontSource, task.CreatedAt, func() int64 {
		if o.session == o.front {
			return o.cursor
		}
		return 0
	}())
}

func projectActivityPost(ctx context.Context, tx pgx.Tx, o projectionOwner, post uuid.UUID, tasks []activityTask, omitted int) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT payload FROM slack_posts WHERE id=$1 FOR NO KEY UPDATE`, post).Scan(&raw); err != nil {
		return err
	}
	if raw == nil {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	var original []map[string]json.RawMessage
	if err := json.Unmarshal(body["blocks"], &original); err != nil {
		return err
	}
	blocks := []any{}
	// Rebuild only the attached facts; original narrative elements stay unchanged.
	for _, block := range original {
		if string(block["type"]) != `"task_card"` && string(block["block_id"]) != `"helmr_child_overflow"` {
			blocks = append(blocks, block)
		}
	}
	var fallback MessageBody
	if err := json.Unmarshal(raw, &fallback); err != nil {
		return err
	}
	var text strings.Builder
	for _, block := range fallback.Blocks {
		if block.Type == "rich_text" {
			for i, element := range block.Elements {
				if i > 0 {
					text.WriteString("\n")
				}
				for _, part := range element.Elements {
					text.WriteString(quoteLiteral(part.Text))
				}
			}
		}
	}
	type state struct {
		ID                     uuid.UUID
		Title, Status, Details string
	}
	states := make([]state, 0, len(tasks))
	for _, task := range tasks {
		status, details := task.presentation()
		states = append(states, state{task.ID, fmt.Sprintf("%s · Turn %d", task.Name, task.Sequence), status, details})
	}
	serialized, _ := json.Marshal(states)
	hash := sha256.Sum256(serialized)
	revision := hex.EncodeToString(hash[:12])
	for _, s := range states {
		blocks = append(blocks, map[string]any{"type": "task_card", "task_id": s.ID.String(), "block_id": fmt.Sprintf("child:%s:%s:%s", post, revision, s.ID), "title": s.Title, "status": s.Status, "details": richBlock{Type: "rich_text", Elements: []richElement{{Type: "rich_text_section", Elements: []textElement{{Type: "text", Text: s.Details}}}}}})
		text.WriteString("\n" + s.Title + ": " + s.Details)
	}
	if omitted > 0 {
		label := fmt.Sprintf("%d more child tasks in Console", omitted)
		o.session = o.front
		blocks = append(blocks, map[string]any{"type": "context", "block_id": "helmr_child_overflow", "elements": []any{map[string]string{"type": "mrkdwn", "text": "<" + o.consoleLink() + "|" + label + ">"}}})
		text.WriteString("\n" + label)
	}
	body["blocks"], _ = json.Marshal(blocks)
	body["text"], _ = json.Marshal(text.String())
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(payload) > 65536 {
		return errors.New("slack work message exceeds encoded size limit")
	}
	digest := sha256.Sum256(payload)
	_, err = tx.Exec(ctx, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,status=CASE WHEN status='posted' THEN 'pending' ELSE status END WHERE id=$1 AND payload_digest<>$3 AND payload IS NOT NULL AND suppressed_revision=0 AND status NOT IN ('failed','suppressed')`, post, payload, digest[:])
	return err
}
