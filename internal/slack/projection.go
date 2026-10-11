package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type projectionOwner struct {
	installation, channel, environment, session, thread, participant, project uuid.UUID
	front, frontSource                                                        uuid.UUID
	cursor                                                                    int64
	label, name                                                               string
	config                                                                    ProjectionConfig
}

type projectionEvent struct {
	sequence int64
	turn     *uuid.UUID
	kind     string
	data     []byte
	at       time.Time
}

// projectParticipant disposes one ordered source event and its message intents in
// one transaction. The installation gate serializes this with authorization loss
// and reauthorization, which close publications and dispose the old unread prefix.
// Rendering never changes a frozen in-flight mutation.
func projectParticipant(ctx context.Context, pool db.TxBeginner, participant uuid.UUID, config ProjectionConfig) (bool, error) {
	if !config.valid() {
		return false, errors.New("the Slack projection configuration is required")
	}
	progressed := false
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		o := projectionOwner{config: config}
		o.participant = participant
		err := tx.QueryRow(ctx, `SELECT c.installation_id,c.id,p.environment_id,p.session_id,p.thread_id,e.project_id
 FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_channels c ON c.id=t.channel_id JOIN environments e ON e.id=p.environment_id WHERE p.id=$1`, participant).Scan(&o.installation, &o.channel, &o.environment, &o.session, &o.thread, &o.project)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		active, err := agent.LockSlackChannels(ctx, tx, o.environment, []uuid.UUID{o.channel})
		if err != nil {
			return err
		}
		var deleted bool
		if err = tx.QueryRow(ctx, `SELECT t.deleted_at IS NOT NULL OR EXISTS(SELECT 1 FROM slack_posts p WHERE p.thread_id=t.id AND p.role='opening' AND (p.delivery_disposed_at IS NOT NULL OR (t.thread_ts IS NULL AND p.status IN ('failed','suppressed')))),t.front_session_id,src.id FROM slack_threads t JOIN slack_thread_sources src ON src.thread_id=t.id AND src.session_id=t.front_session_id WHERE t.id=$1 FOR NO KEY UPDATE OF t`, o.thread).Scan(&deleted, &o.front, &o.frontSource); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT projected_event_seq FROM slack_thread_sources WHERE id=$1 FOR NO KEY UPDATE`, participant).Scan(&o.cursor); err != nil {
			return err
		}
		var e projectionEvent
		err = tx.QueryRow(ctx, `SELECT seq,turn_id,kind,data,created_at FROM session_events WHERE environment_id=$1 AND session_id=$2 AND seq>$3 ORDER BY seq LIMIT 1`, o.environment, o.session, o.cursor).Scan(&e.sequence, &e.turn, &e.kind, &e.data, &e.at)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if e.data == nil {
			return errors.New("unprojected Slack event payload expired")
		}
		var authorized time.Time
		if err = tx.QueryRow(ctx, `SELECT authorized_at FROM slack_installations WHERE id=$1`, o.installation).Scan(&authorized); err != nil {
			return err
		}
		if active && !deleted && !e.at.Before(authorized) {
			if err = tx.QueryRow(ctx, `SELECT a.name FROM agents a JOIN sessions s ON (s.environment_id,s.agent_id)=(a.environment_id,a.id) WHERE s.environment_id=$1 AND s.id=$2 FOR SHARE OF a`, o.environment, o.session).Scan(&o.name); err != nil {
				return err
			}
			o.label = o.name
			if err = projectEvent(ctx, tx, o, e); err != nil {
				return err
			}
			if err = projectActivity(ctx, tx, o, authorized); err != nil {
				return err
			}
		}
		// Ineligible events receive an explicit non-publication disposition through
		// this cursor; they cannot be reconsidered after a route or authority edit.
		_, err = tx.Exec(ctx, `UPDATE slack_thread_sources SET projected_event_seq=$2 WHERE id=$1`, participant, e.sequence)
		progressed = err == nil
		return err
	})
	return progressed, err
}

func projectEvent(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	switch e.kind {
	case "message.rejected":
		return projectMessageFailure(ctx, tx, o, e)
	case "turn.output":
		if e.turn == nil {
			return errors.New("the Slack output has no Turn")
		}
		return projectContent(ctx, tx, o, e, "intermediate", e.data, false, nil)
	case "turn.completed":
		if e.turn == nil {
			return errors.New("the Slack completion has no Turn")
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()) WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate'`, o.participant, *e.turn); err != nil {
			return err
		}
		var response []byte
		if err := tx.QueryRow(ctx, `SELECT response FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 AND status='completed'`, o.environment, o.session, *e.turn).Scan(&response); err != nil {
			return err
		}
		if response == nil {
			return nil
		}
		return projectContent(ctx, tx, o, e, "response", response, true, nil)
	case "ask.created":
		if o.session != o.front {
			return errors.New("owned work cannot publish a human question")
		}
		var question struct {
			ID       uuid.UUID       `json:"ask_id"`
			Question json.RawMessage `json:"question"`
		}
		if err := json.Unmarshal(e.data, &question); err != nil {
			return err
		}
		if question.ID == uuid.Nil() || e.turn == nil {
			return errors.New("the Slack question has no exact target")
		}
		return projectContent(ctx, tx, o, e, "question", question.Question, false, &question.ID)
	case "ask.responded", "ask.cancelled":
		return projectQuestionState(ctx, tx, o, e)
	case "turn.failed", "turn.interrupted", "turn.cancelled", "session.closed", "session.cancelled":
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()) WHERE thread_source_id=$1 AND role='intermediate' AND ($2::uuid IS NULL OR turn_id=$2)`, o.participant, e.turn); err != nil {
			return err
		}
		label := map[string]string{"turn.failed": "Turn failed.", "turn.interrupted": "Turn interrupted.", "turn.cancelled": "Turn cancelled.", "session.closed": "Session closed.", "session.cancelled": "Session cancelled."}[e.kind]
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": label}})
		return projectContent(ctx, tx, o, e, "lifecycle", content, false, nil)
	}
	return nil
}

// A logical intermediate publication spans adjacent writes from its exact Turn.
// Closure (including an authorization cutoff) starts a fresh publication. All
// continuations reuse the first row's appearance, including ones created later.
func projectContent(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent, role string, content json.RawMessage, empty bool, ask *uuid.UUID) error {
	key := fmt.Sprintf("%s:%d", o.session, e.sequence)
	start := e.sequence
	var appearance map[string]json.RawMessage
	path := "post"
	var recipientTeam, recipientUser *string
	if role == "intermediate" {
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT p.publication_key,p.source_start_seq,p.payload,p.presentation_path,p.recipient_team_id,p.recipient_user_id FROM slack_posts p
 WHERE p.thread_source_id=$1 AND p.turn_id=$2 AND p.role='intermediate' AND p.continuation_ordinal=0 AND p.suppressed_revision=0 AND p.status NOT IN ('failed','suppressed')
 AND EXISTS(SELECT 1 FROM slack_posts tail WHERE tail.thread_source_id=p.thread_source_id AND tail.publication_key=p.publication_key AND tail.closed_at IS NULL)
 ORDER BY p.seq DESC LIMIT 1`, o.participant, e.turn).Scan(&key, &start, &raw, &path, &recipientTeam, &recipientUser)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if err = json.Unmarshal(raw, &appearance); err != nil {
				return err
			}
		}
	}
	var presentation []any
	if appearance == nil {
		var question []byte
		if ask != nil {
			if err := tx.QueryRow(ctx, `SELECT question FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND id=$3`, o.environment, o.session, ask).Scan(&question); err != nil {
				return err
			}
		}
		var err error
		presentation, err = presentationBlocks(o, e, role, ask, question)
		if err != nil {
			return err
		}
		if o.session == o.front && (role == "intermediate" || role == "response") {
			var root *string
			if err = tx.QueryRow(ctx, `SELECT thread_ts FROM slack_threads WHERE id=$1`, o.thread).Scan(&root); err != nil {
				return err
			}
			if root != nil {
				recipientTeam, recipientUser, err = projectionRecipient(ctx, tx, o, e.turn)
				if err != nil {
					return err
				}
				if recipientUser != nil {
					path = "stream"
				}
			}
		}
	} else {
		var err error
		presentation, err = frozenPresentationBlocks(appearance["blocks"])
		if err != nil {
			return err
		}
	}
	sources := []ContentSource{{Sequence: e.sequence, Content: content}}
	if start < e.sequence {
		rows, err := tx.Query(ctx, `SELECT seq,data FROM session_events WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND kind='turn.output' AND seq>=$4 AND seq<=$5 ORDER BY seq`, o.environment, o.session, e.turn, start, e.sequence)
		if err != nil {
			return err
		}
		sources, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (ContentSource, error) {
			var s ContentSource
			var raw []byte
			err := row.Scan(&s.Sequence, &raw)
			s.Content = raw
			return s, err
		})
		if err != nil {
			return err
		}
	}
	var rendered []ContentPost
	var err error
	if role == "question" {
		rendered, err = renderQuestion(e.sequence, content)
	} else {
		rendered, err = RenderContent(sources, empty)
	}
	if err != nil {
		return err
	}
	for ordinal, part := range rendered {
		body, err := json.Marshal(part.Body)
		if err != nil {
			return err
		}
		var payload map[string]json.RawMessage
		if err = json.Unmarshal(body, &payload); err != nil {
			return err
		}
		blocks := make([]any, 0, len(presentation)+len(part.Body.Blocks))
		if len(presentation) > 0 {
			blocks = append(blocks, presentation[0])
		}
		for _, block := range part.Body.Blocks {
			blocks = append(blocks, block)
		}
		if len(presentation) > 1 && (role != "question" || ordinal == len(rendered)-1) {
			blocks = append(blocks, presentation[1:]...)
		}
		payload["blocks"], _ = json.Marshal(blocks)
		label := o.label + " · Helmr Agent"
		if o.session != o.front {
			label = "Delegated work · " + o.label
		}
		payload["text"], _ = json.Marshal(label + "\n" + part.Body.Text)
		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM slack_posts WHERE thread_source_id=$1 AND publication_key=$2 AND continuation_ordinal=$3 FOR NO KEY UPDATE`, o.participant, key, ordinal).Scan(&existing)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,
 source_end_seq=$4,source_end_offset=$5,source_digest=$6,status=CASE WHEN status='posted' THEN 'pending' ELSE status END
 WHERE id=$1 AND payload_digest<>$3 AND closed_at IS NULL AND suppressed_revision=0 AND status NOT IN ('failed','suppressed')`, existing, body, digest[:], part.EndSequence, part.EndOffset, part.SourceDigest[:])
		} else if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,publication_key,continuation_ordinal,role,turn_id,ask_id,
 source_start_seq,source_start_offset,source_end_seq,source_end_offset,source_digest,payload,payload_digest,presentation_path,closed_at,recipient_team_id,recipient_user_id)
 SELECT $1,$2,$3,$4,(SELECT thread_id FROM slack_thread_sources WHERE id=$4),COALESCE(max(seq),0)+1,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,CASE WHEN $7='intermediate' AND $18 THEN NULL ELSE clock_timestamp() END,$19,$20 FROM slack_posts WHERE thread_source_id=$4`, uuid.NewV7(), o.environment, o.session, o.participant, key, ordinal, role, e.turn, ask, part.StartSequence, part.StartOffset, part.EndSequence, part.EndOffset, part.SourceDigest[:], body, digest[:], path, ordinal == len(rendered)-1, recipientTeam, recipientUser)
		}
		if err != nil {
			return err
		}
		if ordinal < len(rendered)-1 {
			if _, err = tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()) WHERE thread_source_id=$1 AND publication_key=$2 AND continuation_ordinal=$3`, o.participant, key, ordinal); err != nil {
				return err
			}
		}
	}

	return err
}
