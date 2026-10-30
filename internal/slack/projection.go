package slack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type projectionOwner struct {
	installation, channel, environment, session, thread, participant uuid.UUID
	front, frontSource                                               uuid.UUID
	cursor                                                           int64
	label, name, project, environmentRef                             string
	config                                                           ProjectionConfig
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
		err := tx.QueryRow(ctx, `SELECT c.installation_id,c.id,p.environment_id,p.session_id,p.thread_id,pr.slug,e.slug
 FROM slack_thread_sources p JOIN slack_threads t ON t.id=p.thread_id JOIN slack_channel_bindings c ON c.id=t.slack_channel_binding_id JOIN environments e ON e.id=p.environment_id JOIN projects pr ON pr.id=e.project_id WHERE p.id=$1`, participant).Scan(&o.installation, &o.channel, &o.environment, &o.session, &o.thread, &o.project, &o.environmentRef)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		active, err := slackroute.LockChannels(ctx, tx, o.environment, []uuid.UUID{o.channel})
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
			if active && !deleted {
				var authorized time.Time
				if err = tx.QueryRow(ctx, `SELECT authorized_at FROM slack_installations WHERE id=$1`, o.installation).Scan(&authorized); err != nil {
					return err
				}
				progressed, err = projectControlState(ctx, tx, o, authorized)
				if err != nil || !progressed {
					return err
				}
				return dbnotify.Notify(ctx, tx, "slack_delivery")
			}
			if _, err = tx.Exec(ctx, `UPDATE slack_threads t SET projected_lifecycle_state=`+lifecycleStateSQL+` FROM sessions s WHERE t.id=$1 AND s.environment_id=$2 AND s.id=t.front_session_id`, o.thread, o.environment); err != nil {
				return err
			}
			// Inactive sources still dispose observed control state so they cannot
			// occupy discovery batches forever or replay it after reauthorization.
			progressed, err = slackroute.AdvanceControlProjection(ctx, tx, o.participant, o.environment, o.session)
			return err
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
			if err = tx.QueryRow(ctx, `SELECT a.key FROM agents a JOIN sessions s ON (s.environment_id,s.agent_id)=(a.environment_id,a.id) WHERE s.environment_id=$1 AND s.id=$2 FOR SHARE OF a`, o.environment, o.session).Scan(&o.name); err != nil {
				return err
			}
			o.label = o.name
			if err = projectEvent(ctx, tx, o, e); err != nil {
				return err
			}
			// The current event is already projected within this transaction.
			o.cursor = e.sequence
			if _, err = projectControlState(ctx, tx, o, authorized); err != nil {
				return err
			}
		}
		// Ineligible events receive an explicit non-publication disposition through
		// this cursor; they cannot be reconsidered after a route or authority edit.
		err = slackroute.AdvanceProjection(ctx, tx, participant, e.sequence)
		progressed = err == nil
		if err != nil {
			return err
		}
		return dbnotify.Notify(ctx, tx, "slack_delivery")
	})
	return progressed, err
}

func projectEvent(ctx context.Context, tx pgx.Tx, o projectionOwner, e projectionEvent) error {
	// Child transcript stays in Console. Its lifecycle is projected into the
	// front's existing work messages, never relabelled as front-authored prose.
	if o.session != o.front {
		return nil
	}

	switch e.kind {
	case "session.recovery_gap", "session.storage_pressure":
		return projectRecoveryNotice(ctx, tx, o, e)
	case "message.rejected":
		return projectMessageFailure(ctx, tx, o, e)
	case "turn.output":
		if e.turn == nil {
			return errors.New("the Slack output has no Turn")
		}
		// A terminal publication disposed the unread output prefix under the same
		// installation/thread gates as admission. Later output may start fresh.
		var disposed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate' AND delivery_disposed_through_seq>=$3)`, o.participant, e.turn, e.sequence).Scan(&disposed); err != nil {
			return err
		}
		if disposed {
			return nil
		}
		return projectContent(ctx, tx, o, e, "intermediate", e.data, false, nil)
	case "turn.completed":
		if e.turn == nil {
			return errors.New("the Slack completion has no Turn")
		}
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()),next_attempt_at=CASE WHEN error='rate_limited' THEN next_attempt_at ELSE LEAST(next_attempt_at,clock_timestamp()) END WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate'`, o.participant, *e.turn); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()),next_attempt_at=CASE WHEN error='rate_limited' THEN next_attempt_at ELSE LEAST(next_attempt_at,clock_timestamp()) END WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate'`, o.participant, e.turn); err != nil {
			return err
		}
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
	case "ask.answered", "ask.cancelled":
		return projectQuestionState(ctx, tx, o, e)
	case "turn.failed", "turn.interrupted", "turn.cancelled", "session.closed", "session.cancelled":
		if _, err := tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()),next_attempt_at=CASE WHEN error='rate_limited' THEN next_attempt_at ELSE LEAST(next_attempt_at,clock_timestamp()) END WHERE thread_source_id=$1 AND role='intermediate' AND ($2::uuid IS NULL OR turn_id=$2)`, o.participant, e.turn); err != nil {
			return err
		}
		text := "Execution failed. View details in Console."
		if e.kind == "turn.interrupted" {
			var cause session.ComputerLossInterruption
			if err := json.Unmarshal(e.data, &cause); err != nil {
				return err
			}
			// The event-time gap fact survives acknowledgement before projection.
			// Its existing notice already asks the user to continue.
			if cause.Reason != session.ComputerExecutionLost || cause.RecoveryGap {
				return nil
			}
			text = "This request was interrupted because its work environment was lost. Mention me to try again."
		} else if e.kind != "turn.failed" {
			return nil
		}
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
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
		var exhausted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_posts WHERE thread_source_id=$1 AND post_key=$2)`, o.participant, "overflow:"+e.turn.String()).Scan(&exhausted); err != nil {
			return err
		}
		if exhausted {
			return nil
		}
		var raw []byte
		err := tx.QueryRow(ctx, `SELECT p.post_key,p.source_start_seq,p.payload,p.presentation_path,p.recipient_team_id,p.recipient_user_id FROM slack_posts p
 WHERE p.thread_source_id=$1 AND p.turn_id=$2 AND p.role='intermediate' AND p.continuation_ordinal=0 AND p.suppressed_revision=0 AND p.status NOT IN ('failed','suppressed')
 AND EXISTS(SELECT 1 FROM slack_posts tail WHERE tail.thread_source_id=p.thread_source_id AND tail.post_key=p.post_key AND tail.closed_at IS NULL)
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
		if o.session == o.front && role == "response" {
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
	} else if role != "intermediate" {
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
		if role == "intermediate" {
			rendered, err = renderCanonicalContentLimit(sources, "", 6000)
		} else {
			rendered, err = RenderContent(sources, empty)
		}
	}
	if err != nil {
		return err
	}
	overflow := false
	if role == "intermediate" {
		// Three narrative messages leave the fourth slot for one explicit escape
		// to the complete Console output, even across arbitrarily many asks.
		var other int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM slack_posts WHERE thread_source_id=$1 AND turn_id=$2 AND role='intermediate' AND post_key<>$3`, o.participant, e.turn, key).Scan(&other); err != nil {
			return err
		}
		remaining := max(0, 3-other)
		if len(rendered) > remaining {
			rendered = rendered[:remaining]
			overflow = true
		}
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
		for _, block := range part.Body.Blocks {
			blocks = append(blocks, block)
		}
		if role != "question" || ordinal == len(rendered)-1 {
			blocks = append(blocks, presentation...)
		}
		payload["blocks"], _ = json.Marshal(blocks)
		payload["text"], _ = json.Marshal(part.Body.Text)

		body, err = json.Marshal(payload)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		var existing uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM slack_posts WHERE thread_source_id=$1 AND post_key=$2 AND continuation_ordinal=$3 FOR NO KEY UPDATE`, o.participant, key, ordinal).Scan(&existing)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE slack_posts SET payload=$2,payload_digest=$3,desired_revision=desired_revision+1,
 source_end_seq=$4,source_end_offset=$5,source_digest=$6,status=CASE WHEN status='posted' THEN 'pending' ELSE status END
 WHERE id=$1 AND payload_digest<>$3 AND closed_at IS NULL AND suppressed_revision=0 AND status NOT IN ('failed','suppressed')`, existing, body, digest[:], part.EndSequence, part.EndOffset, part.SourceDigest[:])
		} else if errors.Is(err, pgx.ErrNoRows) {
			_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,seq,post_key,continuation_ordinal,role,turn_id,ask_id,
 source_start_seq,source_start_offset,source_end_seq,source_end_offset,source_digest,payload,payload_digest,presentation_path,closed_at,recipient_team_id,recipient_user_id)
 SELECT $1,$2,$3,$4,(SELECT thread_id FROM slack_thread_sources WHERE id=$4),COALESCE(max(seq),0)+1,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,CASE WHEN $7='intermediate' AND $18 THEN NULL ELSE clock_timestamp() END,$19,$20 FROM slack_posts WHERE thread_source_id=$4`, uuid.NewV7(), o.environment, o.session, o.participant, key, ordinal, role, e.turn, ask, part.StartSequence, part.StartOffset, part.EndSequence, part.EndOffset, part.SourceDigest[:], body, digest[:], path, ordinal == len(rendered)-1, recipientTeam, recipientUser)
		}
		if err != nil {
			return err
		}
		if ordinal < len(rendered)-1 {
			if _, err = tx.Exec(ctx, `UPDATE slack_posts SET closed_at=COALESCE(closed_at,clock_timestamp()),next_attempt_at=CASE WHEN error='rate_limited' THEN next_attempt_at ELSE LEAST(next_attempt_at,clock_timestamp()) END WHERE thread_source_id=$1 AND post_key=$2 AND continuation_ordinal=$3`, o.participant, key, ordinal); err != nil {
				return err
			}
		}
	}

	if overflow {
		return projectWorkOverflow(ctx, tx, o, e)
	}
	return err
}
