package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type ProjectionConfig struct {
	PublicURL  *url.URL
	ControlKey []byte
}

func (c ProjectionConfig) valid() bool {
	return c.PublicURL != nil && c.PublicURL.Host != "" && (c.PublicURL.Scheme == "https" || c.PublicURL.Scheme == "http") && c.PublicURL.User == nil && c.PublicURL.RawQuery == "" && c.PublicURL.Fragment == "" && len(c.ControlKey) >= 32
}

func (o projectionOwner) consoleLink() string {
	target := o.config.PublicURL.JoinPath("sessions", o.session.String())
	target.RawQuery = url.Values{"project_id": {o.project.String()}, "environment_id": {o.environment.String()}}.Encode()
	return target.String()
}

// Resolve only an actual authenticated human who admitted this exact Turn. An
// API key, schedule owner or runtime caller is not a substitute for that human.
// A missing mapping deliberately selects the recipient-less presentation path.
func projectionRecipient(ctx context.Context, tx pgx.Tx, o projectionOwner, turn *uuid.UUID) (*string, *string, error) {
	if turn == nil {
		return nil, nil, nil
	}
	var team, user string
	err := tx.QueryRow(ctx, `SELECT l.team_id,l.slack_user_id FROM turns t
 JOIN slack_installations i ON i.id=$4 JOIN environments e ON e.id=t.environment_id AND e.org_id=i.organization_id
 JOIN slack_user_links l ON l.user_id=t.caller_id AND l.team_id=i.team_id
 JOIN org_members m ON m.org_id=e.org_id AND m.user_id=l.user_id JOIN users u ON u.id=l.user_id
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3 AND t.caller_kind='user' AND m.disabled_at IS NULL AND u.disabled_at IS NULL
 FOR SHARE OF l,m,u`, o.environment, o.session, turn, o.installation).Scan(&team, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &team, &user, nil
}

func presentationBlocks(o projectionOwner, e projectionEvent, role string, ask *uuid.UUID, question json.RawMessage) ([]any, error) {
	label := o.label + " · Helmr Agent"
	if o.front != uuid.Nil() && o.session != o.front {
		label = "Delegated work · " + o.label
	}
	blocks := []any{map[string]any{"type": "context", "elements": []any{plainText(label)}}}
	if role != "question" || ask == nil || e.turn == nil {
		return blocks, nil
	}
	claims := controlEnvelope{Nonce: uuid.NewV7(), Action: "open_answer", Target: controlTarget{Installation: o.installation, Thread: o.thread, Participant: o.participant, Environment: o.environment, Session: o.session, Turn: *e.turn, Ask: *ask}, ExpiresAt: e.at.Add(30 * 24 * time.Hour).Unix()}
	answer, err := encodeControl(o.config.ControlKey, claims)
	if err != nil {
		return nil, err
	}
	if _, err = answerModal(question, answer); err == nil {
		button := map[string]any{"type": "button", "text": plainText("Answer"), "action_id": "helmr.open_answer", "value": answer}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []any{button}})
	} else if !errors.Is(err, errCLIAnswer) {
		return nil, err
	}
	// Even a question that fits a modal may need an answer longer than Slack allows.
	// Keep the command separate from the question's untrusted prompt and values.
	origin := (&url.URL{Scheme: o.config.PublicURL.Scheme, Host: o.config.PublicURL.Host}).String()
	quotedOrigin := "'" + strings.ReplaceAll(origin, "'", "'\"'\"'") + "'"
	target := fmt.Sprintf("%s %s %s --project %s --env %s", o.session, *e.turn, *ask, o.project, o.environment)
	guide := "Answer with the Helmr CLI if this question or your answer does not fit Slack. These commands use your Helmr login; unset HELMR_API_KEY first.\n" +
		"helmr login " + quotedOrigin + "\n" +
		"helmr --api-url " + quotedOrigin + " session turn ask get " + target + " --json\n" +
		"Save your answer in answer.json: a JSON string for text, or a choice object with a selected array of {id, value} entries in declaration order. Include optional text only when allowText is enabled. Replace RESPONSE_ID with a unique ID; reuse it with the same answer for retries.\n" +
		"helmr --api-url " + quotedOrigin + " session turn ask respond " + target + " --answer-file answer.json --response-id RESPONSE_ID"
	blocks = append(blocks, map[string]any{"type": "section", "block_id": "helmr_answer_cli", "text": plainText(guide)})
	return blocks, nil
}

func frozenPresentationBlocks(raw json.RawMessage) ([]any, error) {
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, err
	}
	var result []any
	for _, block := range blocks {
		var kind string
		if err := json.Unmarshal(block["type"], &kind); err != nil {
			return nil, err
		}
		if kind != "rich_text" {
			result = append(result, block)
		}
	}
	return result, nil
}

func renderQuestion(sequence int64, raw json.RawMessage) ([]ContentPost, error) {
	q, err := parseFormQuestion(raw)
	if err != nil {
		return nil, err
	}
	var parts []json.RawMessage
	if err = json.Unmarshal(q.Prompt, &parts); err != nil {
		return nil, err
	}
	if len(q.Answer.Options) > 0 {
		var choices strings.Builder
		choices.WriteString("\nChoices:")
		for _, option := range q.Answer.Options {
			choices.WriteString("\n" + option.Label)
			if option.Description != "" {
				choices.WriteString(" — " + option.Description)
			}
		}
		part, _ := json.Marshal(map[string]string{"type": "text", "text": choices.String()})
		parts = append(parts, part)
	}
	content, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	return renderCanonicalContent([]ContentSource{{Sequence: sequence, Content: content}}, "Question awaiting your answer.")
}
