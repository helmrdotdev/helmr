package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/content"
)

var errCLIAnswer = errors.New("question requires CLI answer")

type formQuestion struct {
	Prompt      json.RawMessage `json:"prompt"`
	Title       string          `json:"title"`
	Type        string          `json:"type"`
	Multiple    bool            `json:"multiple"`
	AllowCustom bool            `json:"allowCustom"`
	AllowNotes  bool            `json:"allowNotes"`
	Options     []struct {
		ID          string          `json:"id"`
		Label       string          `json:"label"`
		Description string          `json:"description"`
		Value       json.RawMessage `json:"value"`
	} `json:"options"`
}

func parseFormQuestion(raw json.RawMessage) (formQuestion, error) {
	var q formQuestion
	canonical, err := content.Question(raw)
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
	if q.Title != "" {
		blocks = append(blocks, map[string]any{"type": "section", "text": plainText(q.Title)})
	}
	for _, post := range posts {
		for _, block := range post.Body.Blocks {
			blocks = append(blocks, block)
		}
	}
	input := func(id, label string, optional bool, element map[string]any) {
		element["action_id"] = id
		blocks = append(blocks, map[string]any{"type": "input", "block_id": id, "label": plainText(label), "optional": optional, "element": element})
	}
	if q.Type == "choice" {
		options := []any{}
		longDescriptions := false
		for _, option := range q.Options {
			if utf8.RuneCountInString(option.Description) > 75 {
				longDescriptions = true
			}
		}
		var details strings.Builder
		for _, option := range q.Options {
			if strings.TrimSpace(option.Label) == "" || utf8.RuneCountInString(option.Label) > 75 {
				return nil, errCLIAnswer
			}
			item := map[string]any{"text": plainText(option.Label), "value": option.ID}
			if longDescriptions {
				details.WriteString(option.Label + "\n" + option.Description + "\n\n")
			} else if option.Description != "" {
				item["description"] = plainText(option.Description)
			}
			options = append(options, item)
		}
		if longDescriptions {
			// Native option descriptions are bounded; put full details in the
			// modal body rather than truncating them or removing the answer path.
			raw, _ := json.Marshal([]map[string]string{{"type": "text", "text": details.String()}})
			posts, err := RenderContent([]ContentSource{{Sequence: 1, Content: raw}}, false)
			if err != nil {
				return nil, err
			}
			for _, post := range posts {
				for _, block := range post.Body.Blocks {
					blocks = append(blocks, block)
				}
			}
		}
		kind := "static_select"
		if q.Multiple {
			kind = "multi_static_select"
		}
		element := map[string]any{"type": kind, "options": options}
		if len(q.Options) <= 10 && !q.AllowCustom {
			if q.Multiple {
				element["type"] = "checkboxes"
			} else {
				element["type"] = "radio_buttons"
			}
		}
		input("choice", "Selection", q.AllowCustom, element)
	}
	if q.Type == "text" {
		input("text", "Answer", true, map[string]any{"type": "plain_text_input", "multiline": true, "max_length": 3000})
	}
	if q.AllowCustom {
		input("custom", "Custom answer", true, map[string]any{"type": "plain_text_input", "multiline": true, "max_length": 3000})
	}
	if q.AllowNotes {
		input("notes", "Notes", true, map[string]any{"type": "plain_text_input", "multiline": true, "max_length": 3000})
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
	if q.Type == "choice" {
		expected["choice"] = true
	}
	if q.Type == "text" {
		expected["text"] = true
	}
	if q.AllowCustom {
		expected["custom"] = true
	}
	if q.AllowNotes {
		expected["notes"] = true
	}
	if len(state) != len(expected) {
		return nil, content.ErrAnswerInvalid
	}
	for id, entries := range state {
		if !expected[id] || len(entries) != 1 || entries[id] == nil {
			return nil, content.ErrAnswerInvalid
		}
	}
	requiredField := func(block, member string) bool {
		var fields map[string]json.RawMessage
		return json.Unmarshal(state[block][block], &fields) == nil && fields[member] != nil
	}
	texts := map[string]string{}
	for _, key := range []string{"text", "custom", "notes"} {
		if !expected[key] {
			continue
		}
		var field struct {
			Type  string  `json:"type"`
			Value *string `json:"value"`
		}
		if !requiredField(key, "value") || json.Unmarshal(state[key][key], &field) != nil || field.Type != "plain_text_input" {
			return nil, content.ErrAnswerInvalid
		}
		if field.Value != nil {
			texts[key] = *field.Value
		}
	}

	if q.Type == "text" {
		raw, _ := json.Marshal(texts["text"])
		if _, err := content.Answer(question, raw); err != nil {
			return nil, err
		}
		return content.Submission(raw)
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
		return nil, content.ErrAnswerInvalid
	}
	ids := map[string]bool{}
	if q.Multiple {
		if !requiredField("choice", "selected_options") || (field.Type != "multi_static_select" && field.Type != "checkboxes") || field.Selected != nil {
			return nil, content.ErrAnswerInvalid
		}
		for _, item := range field.Selections {
			if ids[item.Value] {
				return nil, content.ErrAnswerInvalid
			}
			ids[item.Value] = true
		}
	} else {
		if !requiredField("choice", "selected_option") || (field.Type != "static_select" && field.Type != "radio_buttons") || len(field.Selections) != 0 {
			return nil, content.ErrAnswerInvalid
		}
		if field.Selected != nil {
			ids[field.Selected.Value] = true
		}
	}
	selected := []string{}
	for _, option := range q.Options {
		if ids[option.ID] {
			selected = append(selected, option.ID)
			delete(ids, option.ID)
		}
	}
	if len(ids) != 0 {
		return nil, content.ErrAnswerInvalid
	}
	answer := map[string]any{"selectedIds": selected}
	for _, key := range []string{"custom", "notes"} {
		if texts[key] != "" {
			answer[key] = texts[key]
		}
	}
	raw, _ := json.Marshal(answer)
	if _, err := content.Answer(question, raw); err != nil {
		return nil, err
	}
	return content.Submission(raw)
}

// Inline submission includes only the choice. Optional private text is entered
// in the scoped modal, never copied from unrelated message state.
func parseInlineAnswer(question json.RawMessage, state formState) (json.RawMessage, error) {
	q, err := parseFormQuestion(question)
	if err != nil {
		return nil, err
	}
	if !inlineChoice(q) {
		return nil, content.ErrAnswerInvalid
	}
	form := formState{"choice": state["choice"]}
	for key, enabled := range map[string]bool{"custom": q.AllowCustom, "notes": q.AllowNotes} {
		if enabled {
			form[key] = map[string]json.RawMessage{key: json.RawMessage(`{"type":"plain_text_input","value":null}`)}
		}
	}
	return parseFormAnswer(question, form)
}
