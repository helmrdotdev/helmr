package slack

import (
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

var errCLIAnswer = errors.New("question requires CLI answer")

type formQuestion struct {
	Prompt json.RawMessage `json:"prompt"`
	Answer struct {
		Type      string `json:"type"`
		Multiple  bool   `json:"multiple"`
		AllowText bool   `json:"allowText"`
		Options   []struct {
			ID          string          `json:"id"`
			Label       string          `json:"label"`
			Description string          `json:"description"`
			Value       json.RawMessage `json:"value"`
		} `json:"options"`
	} `json:"answer"`
}

func parseFormQuestion(raw json.RawMessage) (formQuestion, error) {
	var q formQuestion
	canonical, err := conversation.Question(raw)
	if err != nil {
		return q, err
	}
	err = json.Unmarshal(canonical, &q)
	return q, err
}

func plainText(text string) map[string]any {
	return map[string]any{"type": "plain_text", "text": text, "emoji": false}
}

// answerModal preserves the complete prompt and every option description. Native
// capacity failures require the caller to publish the complete prompt with the
// CLI instructions; they never authorize truncation or removal of choices.
func answerModal(raw json.RawMessage, signedForm string) (json.RawMessage, error) {
	q, err := parseFormQuestion(raw)
	if err != nil {
		return nil, err
	}
	posts, err := RenderContent([]ContentSource{{Sequence: 1, Content: q.Prompt}}, false)
	if err != nil {
		return nil, err
	}
	blocks := []any{}
	for _, post := range posts {
		for _, block := range post.Body.Blocks {
			blocks = append(blocks, block)
		}
	}
	input := func(id, label string, optional bool, element map[string]any) {
		element["action_id"] = id
		blocks = append(blocks, map[string]any{"type": "input", "block_id": id, "label": plainText(label), "optional": optional, "element": element})
	}
	if q.Answer.Type == "choice" {
		options := []any{}
		for _, option := range q.Answer.Options {
			if option.Label == "" || utf8.RuneCountInString(option.Label) > 75 || utf8.RuneCountInString(option.Description) > 75 {
				return nil, errCLIAnswer
			}
			item := map[string]any{"text": plainText(option.Label), "value": option.ID}
			if option.Description != "" {
				item["description"] = plainText(option.Description)
			}
			options = append(options, item)
		}
		kind := "static_select"
		if q.Answer.Multiple {
			kind = "multi_static_select"
		}
		input("choice", "Selection", q.Answer.AllowText, map[string]any{"type": kind, "options": options})
	}
	if q.Answer.Type == "text" || q.Answer.AllowText {
		input("text", "Answer", true, map[string]any{"type": "plain_text_input", "multiline": true, "max_length": 3000})
	}
	if len(blocks) > 100 {
		return nil, errCLIAnswer
	}
	view, err := json.Marshal(map[string]any{"type": "modal", "callback_id": "helmr.answer", "private_metadata": signedForm, "title": plainText("Answer question"), "submit": plainText("Submit answer"), "close": plainText("Cancel"), "blocks": blocks})
	if len(view) > 250000 {
		return nil, errCLIAnswer
	}
	return view, err
}

type formState map[string]map[string]json.RawMessage

// parseFormAnswer uses IDs only as references into the immutable question. Slack
// values never supply application answer values or alter declaration ordering.
func parseFormAnswer(question json.RawMessage, state formState) (json.RawMessage, error) {
	q, err := parseFormQuestion(question)
	if err != nil {
		return nil, err
	}
	expected := map[string]bool{}
	if q.Answer.Type == "choice" {
		expected["choice"] = true
	}
	if q.Answer.Type == "text" || q.Answer.AllowText {
		expected["text"] = true
	}
	if len(state) != len(expected) {
		return nil, conversation.ErrAnswerInvalid
	}
	for id, entries := range state {
		if !expected[id] || len(entries) != 1 || entries[id] == nil {
			return nil, conversation.ErrAnswerInvalid
		}
	}
	requiredField := func(block, member string) bool {
		var fields map[string]json.RawMessage
		return json.Unmarshal(state[block][block], &fields) == nil && fields[member] != nil
	}
	text := ""
	if expected["text"] {
		var field struct {
			Type  string  `json:"type"`
			Value *string `json:"value"`
		}
		if !requiredField("text", "value") || json.Unmarshal(state["text"]["text"], &field) != nil || field.Type != "plain_text_input" {
			return nil, conversation.ErrAnswerInvalid
		}
		if field.Value != nil {
			text = *field.Value
		}
	}
	if q.Answer.Type == "text" {
		raw, _ := json.Marshal(text)
		return conversation.Answer(question, raw)
	}
	var field struct {
		Type     string `json:"type"`
		Selected *struct {
			Value string `json:"value"`
		} `json:"selected_option"`
		Selections []struct {
			Value string `json:"value"`
		} `json:"selected_options"`
	}
	if json.Unmarshal(state["choice"]["choice"], &field) != nil {
		return nil, conversation.ErrAnswerInvalid
	}
	ids := map[string]bool{}
	if q.Answer.Multiple {
		if !requiredField("choice", "selected_options") || field.Type != "multi_static_select" || field.Selected != nil {
			return nil, conversation.ErrAnswerInvalid
		}
		for _, item := range field.Selections {
			if ids[item.Value] {
				return nil, conversation.ErrAnswerInvalid
			}
			ids[item.Value] = true
		}
	} else {
		if !requiredField("choice", "selected_option") || field.Type != "static_select" || len(field.Selections) != 0 {
			return nil, conversation.ErrAnswerInvalid
		}
		if field.Selected != nil {
			ids[field.Selected.Value] = true
		}
	}
	selected := []any{}
	for _, option := range q.Answer.Options {
		if ids[option.ID] {
			selected = append(selected, map[string]any{"id": option.ID, "value": option.Value})
			delete(ids, option.ID)
		}
	}
	if len(ids) != 0 {
		return nil, conversation.ErrAnswerInvalid
	}
	answer := map[string]any{"selected": selected}
	if text != "" {
		answer["text"] = text
	}
	raw, _ := json.Marshal(answer)
	return conversation.Answer(question, raw)
}
