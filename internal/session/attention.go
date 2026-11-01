package session

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"github.com/helmrdotdev/helmr/internal/auth"
	"strings"
	"time"
	"unicode"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// AttentionLimit bounds the items of each kind one attention read returns; the
// counts still report every waiting item.
const AttentionLimit = 100

// attentionSummaryRunes bounds the one-line summary of a waiting Question.
const attentionSummaryRunes = 200

// AttentionItem is one thing in an Environment waiting on a person.
type AttentionItem struct {
	ID        uuid.UUID
	SessionID uuid.UUID
	TurnID    *uuid.UUID
	Agent     string
	At        time.Time
	// The Question's first text part, the hold's kind, the failed Turn's error
	// code, or why the Session's Slack delivery stopped.
	Summary      string
	TurnSequence *int64
}

// Attention lists, oldest first, what is waiting on a person in one
// Environment: pending Questions, holds on open Sessions, failed Turns that
// still stand and open Sessions whose Slack thread no longer receives output.
// Each list holds at most AttentionLimit items; the counts are complete.
type Attention struct {
	Questions, Holds, FailedTurns, SlackDeliveryStopped                  []AttentionItem
	QuestionCount, HoldCount, FailedTurnCount, SlackDeliveryStoppedCount int64
}

// A failed Turn waits on a person while it is the latest Turn of a Session that
// is still open, for 24 hours after it failed. It leaves the list when the
// Session receives another Turn, closes or is cancelled, or the window passes;
// there is no separate dismissal.
const attentionFailedTurnWindow = `interval '24 hours'`

const attentionAccessSQL = `($2::boolean OR s.agent_id=ANY($3::uuid[]))`

const attentionQuestionsFromSQL = `
 FROM turn_asks a
 JOIN sessions s ON (s.environment_id,s.id)=(a.environment_id,a.session_id)
 JOIN agents ag ON (ag.environment_id,ag.id)=(s.environment_id,s.agent_id)
 WHERE a.environment_id=$1 AND a.status='pending'
 AND ` + attentionAccessSQL

const attentionQuestionsSQL = `SELECT a.id,a.session_id,a.turn_id,ag.key,a.created_at,a.question,count(*) OVER ()` + attentionQuestionsFromSQL + `
 ORDER BY a.created_at,a.id LIMIT $4`
const attentionQuestionsCountSQL = `SELECT count(*)` + attentionQuestionsFromSQL

const attentionHoldsFromSQL = `
 FROM session_holds h
 JOIN sessions s ON (s.environment_id,s.id)=(h.environment_id,h.session_id)
 JOIN agents ag ON (ag.environment_id,ag.id)=(s.environment_id,s.agent_id)
 WHERE h.environment_id=$1 AND h.released_at IS NULL AND s.status='open'
 AND ` + attentionAccessSQL

const attentionHoldsSQL = `SELECT h.id,h.session_id,ag.key,h.created_at,h.kind,count(*) OVER ()` + attentionHoldsFromSQL + `
 ORDER BY h.created_at,h.id LIMIT $4`
const attentionHoldsCountSQL = `SELECT count(*)` + attentionHoldsFromSQL

const attentionFailedTurnsFromSQL = `
 FROM turns t
 JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.session_id)
 JOIN agents ag ON (ag.environment_id,ag.id)=(s.environment_id,s.agent_id)
 WHERE t.environment_id=$1 AND t.status='failed' AND t.terminal_at>clock_timestamp()-` + attentionFailedTurnWindow + ` AND s.status='open'
 AND NOT EXISTS(SELECT 1 FROM turns later WHERE later.environment_id=t.environment_id AND later.session_id=t.session_id AND later.seq>t.seq)
 AND ` + attentionAccessSQL

const attentionFailedTurnsSQL = `SELECT t.id,t.session_id,ag.key,t.terminal_at,t.seq,t.error_code,count(*) OVER ()` + attentionFailedTurnsFromSQL + `
 ORDER BY t.terminal_at,t.id LIMIT $4`
const attentionFailedTurnsCountSQL = `SELECT count(*)` + attentionFailedTurnsFromSQL

// Slack delivery to a Session's thread stops when its opening is disposed as
// unconfirmed or ended before a root was known; later output is suppressed while
// the Session keeps working. It waits on a person until the Session closes.
const attentionSlackDeliveryStoppedFromSQL = `
 FROM slack_threads th
 JOIN slack_posts o ON o.thread_id=th.id AND o.role='opening'
 JOIN sessions s ON (s.environment_id,s.id)=(th.environment_id,th.front_session_id)
 JOIN agents ag ON (ag.environment_id,ag.id)=(s.environment_id,s.agent_id)
 WHERE th.environment_id=$1 AND s.status='open'
 AND (o.delivery_disposed_at IS NOT NULL OR (th.thread_ts IS NULL AND o.status IN ('failed','suppressed')))
 AND ` + attentionAccessSQL

const attentionSlackDeliveryStoppedSQL = `SELECT o.id,s.id,ag.key,COALESCE(o.delivery_disposed_at,o.closed_at,o.created_at) AS stopped_at,COALESCE(o.error,''),count(*) OVER ()` + attentionSlackDeliveryStoppedFromSQL + `
 ORDER BY stopped_at,o.id LIMIT $4`
const attentionSlackDeliveryStoppedCountSQL = `SELECT count(*)` + attentionSlackDeliveryStoppedFromSQL

// ListAttention reads the Environment's waiting work with the same authority as
// reading its Sessions, in one snapshot.
func ListAttention(ctx context.Context, auditPolicy auditpolicy.Policy, pool db.TxBeginner, caller Caller, env uuid.UUID) (Attention, error) {
	result := Attention{Questions: []AttentionItem{}, Holds: []AttentionItem{}, FailedTurns: []AttentionItem{}, SlackDeliveryStopped: []AttentionItem{}}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		if err := authorizeRead(ctx, tx, caller, env); err != nil {
			return err
		}
		filter, err := readScopeFor(ctx, tx, caller, env)
		if err != nil {
			return err
		}
		if !filter.All && len(filter.Agents) == 0 {
			return ErrDenied
		}
		result.Questions, result.QuestionCount, err = attentionRows(ctx, tx, attentionQuestionsSQL, env, filter, func(row pgx.Rows, item *AttentionItem, count *int64) error {
			var turn uuid.UUID
			var question []byte
			if err := row.Scan(&item.ID, &item.SessionID, &turn, &item.Agent, &item.At, &question, count); err != nil {
				return err
			}
			item.TurnID = &turn
			item.Summary = questionSummary(question)
			return nil
		})
		if err != nil {
			return err
		}
		result.Holds, result.HoldCount, err = attentionRows(ctx, tx, attentionHoldsSQL, env, filter, func(row pgx.Rows, item *AttentionItem, count *int64) error {
			return row.Scan(&item.ID, &item.SessionID, &item.Agent, &item.At, &item.Summary, count)
		})
		if err != nil {
			return err
		}
		result.FailedTurns, result.FailedTurnCount, err = attentionRows(ctx, tx, attentionFailedTurnsSQL, env, filter, func(row pgx.Rows, item *AttentionItem, count *int64) error {
			var sequence int64
			var code *string
			if err := row.Scan(&item.ID, &item.SessionID, &item.Agent, &item.At, &sequence, &code, count); err != nil {
				return err
			}
			turn := item.ID
			item.TurnID, item.TurnSequence = &turn, &sequence
			if code != nil {
				item.Summary = *code
			}
			return nil
		})
		if err != nil {
			return err
		}
		result.SlackDeliveryStopped, result.SlackDeliveryStoppedCount, err = attentionRows(ctx, tx, attentionSlackDeliveryStoppedSQL, env, filter, func(row pgx.Rows, item *AttentionItem, count *int64) error {
			return row.Scan(&item.ID, &item.SessionID, &item.Agent, &item.At, &item.Summary, count)
		})
		if err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		for _, items := range [][]AttentionItem{result.Questions, result.Holds, result.FailedTurns, result.SlackDeliveryStopped} {
			for _, item := range items {
				if seen[item.SessionID] {
					continue
				}
				seen[item.SessionID] = true
				if err := auditOperation(ctx, tx, auditPolicy, caller, auth.PermissionSessionsRead, audit.Event{EnvironmentID: env, SessionID: item.SessionID, Action: "session.content_read", TargetKind: "session", TargetID: item.SessionID, Details: audit.LifecycleDetails{}}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return Attention{}, hideMissing(err)
	}
	return result, hideMissing(err)
}

func attentionRows(ctx context.Context, tx pgx.Tx, sql string, env uuid.UUID, filter sessionReadScope, scan func(pgx.Rows, *AttentionItem, *int64) error) ([]AttentionItem, int64, error) {
	items := []AttentionItem{}
	var count int64
	rows, err := tx.Query(ctx, sql, env, filter.All, filter.Agents, AttentionLimit)
	if err != nil {
		return items, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item AttentionItem
		if err := scan(rows, &item, &count); err != nil {
			return items, 0, err
		}
		items = append(items, item)
	}
	return items, count, rows.Err()
}

// questionSummary is the Question's first text part on one line, shortened.
// An expired or text-free Question has no summary.
func questionSummary(raw []byte) string {
	var question struct {
		Prompt []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"prompt"`
	}
	if raw == nil || json.Unmarshal(raw, &question) != nil {
		return ""
	}
	for _, part := range question.Prompt {
		if part.Type == "text" {
			return oneLine(part.Text, attentionSummaryRunes)
		}
	}
	return ""
}

// oneLine collapses whitespace and control characters to single spaces and
// shortens the text to limit runes.
func oneLine(text string, limit int) string {
	words := strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	runes := []rune(strings.Join(words, " "))
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return string(runes)
}

type AttentionCounts struct {
	QuestionCount, HoldCount, FailedTurnCount, SlackDeliveryStoppedCount int64
}

// CountAttention reads complete metadata counts without selecting application content.
func CountAttention(ctx context.Context, pool db.TxBeginner, caller Caller, env uuid.UUID) (AttentionCounts, error) {
	var result AttentionCounts
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		if err := authorizeRead(ctx, tx, caller, env); err != nil {
			return err
		}
		filter, err := readScopeFor(ctx, tx, caller, env)
		if err != nil {
			return err
		}
		if !filter.All && len(filter.Agents) == 0 {
			return ErrDenied
		}
		const countsSQL = `SELECT (` + attentionQuestionsCountSQL + `), (` + attentionHoldsCountSQL + `), (` + attentionFailedTurnsCountSQL + `), (` + attentionSlackDeliveryStoppedCountSQL + `)`
		return tx.QueryRow(ctx, countsSQL, env, filter.All, filter.Agents).Scan(&result.QuestionCount, &result.HoldCount, &result.FailedTurnCount, &result.SlackDeliveryStoppedCount)
	})
	if err != nil {
		return AttentionCounts{}, hideMissing(err)
	}
	return result, nil
}
