package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/jackc/pgx/v5"
)

type ProjectionConfig struct {
	AppOrigin  *url.URL
	ControlKey []byte
}

func (c ProjectionConfig) valid() bool {
	return c.AppOrigin != nil && c.AppOrigin.Host != "" && (c.AppOrigin.Scheme == "https" || c.AppOrigin.Scheme == "http") && c.AppOrigin.User == nil && c.AppOrigin.RawQuery == "" && c.AppOrigin.Fragment == "" && len(c.ControlKey) >= 32
}

func (o projectionOwner) consoleLink() string {
	target := o.config.AppOrigin.JoinPath("sessions", o.session.String())
	target.RawQuery = url.Values{"project": {o.project}, "environment": {o.environmentRef}}.Encode()
	return target.String()
}

// Resolve only an actual authenticated human who admitted this exact Turn. An
// API key, schedule owner or runtime caller is not a substitute for that human.
// This is historical presentation only; action admission checks live authority.
// A missing mapping deliberately selects the recipient-less presentation path.
func projectionRecipient(ctx context.Context, tx pgx.Tx, o projectionOwner, turn *uuid.UUID) (*string, *string, error) {
	if turn == nil {
		return nil, nil, nil
	}
	var team, user string
	err := tx.QueryRow(ctx, `SELECT l.team_id,l.slack_user_id FROM turns t
 JOIN slack_installations i ON i.id=$4 JOIN environments e ON e.id=t.environment_id AND e.org_id=i.org_id
 JOIN slack_requests r ON r.environment_id=t.environment_id AND r.session_id=t.session_id AND r.turn_id=t.id
  AND r.installation_id=i.id AND r.user_id=t.caller_id AND r.status='accepted'
  AND r.operation IN ('start','enqueue') AND r.message_id IS NULL
 JOIN slack_user_links l ON l.id=r.source_link_id AND l.user_id=t.caller_id AND l.installation_id=i.id
 JOIN org_members m ON m.id=l.member_id AND m.org_id=e.org_id AND m.user_id=l.user_id
 WHERE t.environment_id=$1 AND t.session_id=$2 AND t.id=$3 AND t.caller_kind='user' AND m.removed_at IS NULL`, o.environment, o.session, turn, o.installation).Scan(&team, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &team, &user, nil
}

func consoleContext(link, label string) any {
	return map[string]any{"type": "context", "elements": []any{map[string]string{"type": "mrkdwn", "text": "<" + link + "|" + label + ">"}}}
}

func shortChoice(q formQuestion) bool {
	if q.Type != "choice" || q.Multiple || len(q.Options) < 1 || len(q.Options) > 4 || len(q.Prompt) > 2000 {
		return false
	}
	for _, option := range q.Options {
		if strings.TrimSpace(option.Label) == "" || utf8.RuneCountInString(option.Label) > 40 || option.Description != "" {
			return false
		}
	}
	return true
}

// Inline choices have no shared draft writes: only the submitter's interaction
// state is admitted when they explicitly press Submit answer.
func inlineChoice(q formQuestion) bool {
	if q.Type != "choice" || len(q.Options) > 10 || len(q.Prompt) > 2000 {
		return false
	}
	for _, option := range q.Options {
		if strings.TrimSpace(option.Label) == "" || utf8.RuneCountInString(option.Label) > 75 || utf8.RuneCountInString(option.Description) > 75 {
			return false
		}
	}
	return true
}

func choiceElement(q formQuestion) map[string]any {
	options := []any{}
	for _, option := range q.Options {
		item := map[string]any{"text": plainText(option.Label), "value": option.ID}
		if option.Description != "" {
			item["description"] = plainText(option.Description)
		}
		options = append(options, item)
	}
	kind := "radio_buttons"
	if q.Multiple {
		kind = "checkboxes"
	}
	return map[string]any{"type": kind, "action_id": "choice", "options": options}
}

func presentationBlocks(o projectionOwner, e projectionEvent, role string, ask *uuid.UUID, question json.RawMessage) ([]any, error) {
	if role != "question" || ask == nil || e.turn == nil {
		if role == "response" || role == "lifecycle" {
			return []any{consoleContext(o.consoleLink(), "View in Console")}, nil
		}
		return nil, nil
	}
	q, err := parseFormQuestion(question)
	if err != nil {
		return nil, err
	}
	claims := controlEnvelope{Nonce: uuid.NewV7(), Action: "open_answer", Target: controlTarget{Installation: o.installation, Thread: o.thread, Participant: o.participant, Environment: o.environment, Session: o.session, Turn: *e.turn, Ask: *ask}, ExpiresAt: e.at.Add(30 * 24 * time.Hour).Unix()}
	buttons := []any{}
	blocks := []any{}
	direct := shortChoice(q)
	if direct {
		for i, option := range q.Options {
			choice := claims
			choice.Nonce = uuid.NewV7()
			choice.Action = "choose"
			choice.OptionID = option.ID
			signed, err := encodeControl(o.config.ControlKey, choice)
			if err != nil {
				return nil, err
			}
			buttons = append(buttons, map[string]any{"type": "button", "text": plainText(option.Label), "action_id": fmt.Sprintf("helmr.choose.%d", i), "value": signed})
		}
	}
	if !direct && inlineChoice(q) {
		blocks = append(blocks, map[string]any{"type": "actions", "block_id": "choice", "elements": []any{choiceElement(q)}})
		submit := claims
		submit.Action = "submit_choices"
		signed, err := encodeControl(o.config.ControlKey, submit)
		if err != nil {
			return nil, err
		}
		buttons = append(buttons, map[string]any{"type": "button", "text": plainText("Submit answer"), "action_id": "helmr.submit_choices", "value": signed})
	}
	if (!direct && !inlineChoice(q)) || q.AllowNotes || q.AllowCustom {
		signed, err := encodeControl(o.config.ControlKey, claims)
		if err != nil {
			return nil, err
		}
		if _, err = answerModal(question, signed); err == nil {
			label := "Answer…"
			if q.AllowCustom && q.AllowNotes {
				label = "Write or add a note…"
			} else if q.AllowCustom {
				label = "Write an answer…"
			} else if q.AllowNotes {
				label = "Answer with a note…"
			} else if q.Type == "text" {
				label = "Write an answer…"
			}
			buttons = append(buttons, map[string]any{"type": "button", "text": plainText(label), "action_id": "helmr.open_answer", "value": signed})
		} else if !errors.Is(err, errCLIAnswer) {
			return nil, err
		}
	}
	if len(buttons) > 0 {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": buttons})
	}
	target := o.config.AppOrigin.JoinPath("sessions", o.session.String(), "turns", e.turn.String(), "asks", ask.String())
	target.RawQuery = url.Values{"project": {o.project}, "environment": {o.environmentRef}}.Encode()
	blocks = append(blocks, consoleContext(target.String(), "View question and CLI instructions"))
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
	if q.Title != "" {
		title, _ := json.Marshal(map[string]string{"type": "text", "text": q.Title + "\n\n"})
		parts = append([]json.RawMessage{title}, parts...)
	}
	var choices strings.Builder
	if len(q.Options) > 0 {
		choices.WriteString("\nChoices:")
		for _, option := range q.Options {
			choices.WriteString("\n" + option.Label)
			if option.Description != "" {
				choices.WriteString(" — " + option.Description)
			}
		}
		part, _ := json.Marshal(map[string]string{"type": "text", "text": choices.String()})
		if !shortChoice(q) && !inlineChoice(q) {
			parts = append(parts, part)
		}
	}
	content, err := json.Marshal(parts)
	if err != nil {
		return nil, err
	}
	posts, err := renderCanonicalContent([]ContentSource{{Sequence: sequence, Content: content}}, "Question awaiting your answer.")
	if err == nil && len(posts) == 1 && (shortChoice(q) || inlineChoice(q)) {
		// Notifications and assistive clients retain the choices without a second
		// visible list alongside the interactive controls.
		posts[0].Body.Text += choices.String()
	}
	return posts, err
}
