package content

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const ChoiceTextBytes = 8 * 1024

var ErrAnswerInvalid = errors.New("answer does not match its declared question")

type choiceOption struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}
type questionDefinition struct {
	Type        string
	Options     []choiceOption
	Multiple    bool
	AllowCustom bool
	AllowNotes  bool
}

func objectFields(raw json.RawMessage, required []string, optional ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil, ErrInvalid
	}
	allowed := map[string]bool{}
	for _, key := range required {
		if fields[key] == nil {
			return nil, ErrInvalid
		}
		allowed[key] = true
	}
	for _, key := range optional {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return nil, ErrInvalid
		}
	}
	return fields, nil
}
func stringField(raw json.RawMessage) (string, error) {
	var text *string
	if json.Unmarshal(raw, &text) != nil || text == nil {
		return "", ErrInvalid
	}
	return *text, nil
}
func boolField(raw json.RawMessage) (bool, error) {
	if raw == nil {
		return false, nil
	}
	var value *bool
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return false, ErrInvalid
	}
	return *value, nil
}

// Question validates a complete question before it can consume budget or
// become visible. In particular every configured choice must be answerable.
func Question(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(canonical) > ContentBytes {
		return nil, fmt.Errorf("%w: Question exceeds 256 KiB", ErrLimit)
	}
	fields, err := objectFields(canonical, []string{"prompt", "type"}, "title", "options", "multiple", "allowCustom", "allowNotes")
	if err != nil {
		return nil, err
	}
	if title := fields["title"]; title != nil {
		text, err := stringField(title)
		if err != nil || strings.TrimSpace(text) == "" {
			return nil, ErrInvalid
		}
		if len(text) > 256 {
			return nil, ErrLimit
		}
	}
	if _, err := Content(fields["prompt"], false); err != nil {
		return nil, err
	}
	if _, err := parseQuestion(canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}
func parseQuestion(raw json.RawMessage) (questionDefinition, error) {
	fields, err := objectFields(raw, []string{"prompt", "type"}, "title", "options", "multiple", "allowCustom", "allowNotes")
	if err != nil {
		return questionDefinition{}, err
	}
	kind, err := stringField(fields["type"])
	if err != nil {
		return questionDefinition{}, err
	}
	result := questionDefinition{Type: kind}
	if kind == "text" {
		if fields["options"] != nil || fields["multiple"] != nil || fields["allowCustom"] != nil || fields["allowNotes"] != nil {
			return result, ErrInvalid
		}
		return result, nil
	}
	if kind != "choice" {
		return result, ErrInvalid
	}
	var options []json.RawMessage
	if json.Unmarshal(fields["options"], &options) != nil || len(options) == 0 {
		return result, ErrInvalid
	}
	if len(options) > 100 {
		return result, fmt.Errorf("%w: Choice exceeds 100 options", ErrLimit)
	}
	if result.Multiple, err = boolField(fields["multiple"]); err != nil {
		return result, err
	}
	if result.AllowCustom, err = boolField(fields["allowCustom"]); err != nil {
		return result, err
	}
	if result.AllowNotes, err = boolField(fields["allowNotes"]); err != nil {
		return result, err
	}
	seen := map[string]bool{}
	for _, rawOption := range options {
		option, err := objectFields(rawOption, []string{"id", "label", "value"}, "description")
		if err != nil {
			return result, err
		}
		id, err := stringField(option["id"])
		if err != nil || id == "" || seen[id] {
			return result, ErrInvalid
		}
		label, err := stringField(option["label"])
		if err != nil {
			return result, err
		}
		if len(id) > 128 || len(label) > 256 {
			return result, fmt.Errorf("%w: Choice ID exceeds 128 bytes or label exceeds 256 bytes", ErrLimit)
		}
		if description := option["description"]; description != nil {
			text, err := stringField(description)
			if err != nil {
				return result, err
			}
			if len(text) > 2048 {
				return result, fmt.Errorf("%w: Choice description exceeds 2 KiB", ErrLimit)
			}
		}
		seen[id] = true
		result.Options = append(result.Options, choiceOption{ID: id, Value: option["value"]})
	}
	maxBytes := 0
	selections := [][]choiceOption{result.Options}
	if !result.Multiple {
		selections = nil
		for _, option := range result.Options {
			selections = append(selections, []choiceOption{option})
		}
	}
	for _, selected := range selections {
		raw, _ := json.Marshal(map[string]any{"selected": selected})
		canonical, err := jsoncanon.Transform(raw)
		if err != nil {
			return result, ErrInvalid
		}
		if len(canonical) > maxBytes {
			maxBytes = len(canonical)
		}
	}
	if result.AllowCustom {
		if result.Multiple {
			maxBytes += len(`,"custom":`) + ChoiceTextBytes
		} else {
			maxBytes = max(maxBytes, len(`{"selected":[],"custom":}`)+ChoiceTextBytes)
		}
	}
	if result.AllowNotes {
		maxBytes += len(`,"notes":`) + ChoiceTextBytes
	}
	if maxBytes > AnswerBytes {
		return result, fmt.Errorf("%w: Configured choice answer exceeds 64 KiB", ErrLimit)
	}
	return result, nil
}

// Submission defines replay identity without depending on retained option payloads.
// Choice order is immaterial; the resolved answer uses declaration order.
func Submission(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrAnswerInvalid
	}
	if len(canonical) > AnswerBytes {
		return nil, ErrLimit
	}
	if _, err := stringField(canonical); err == nil {
		return canonical, nil
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(canonical, &envelope) == nil {
		for _, key := range []string{"custom", "notes"} {
			if len(envelope[key]) > ChoiceTextBytes {
				return nil, ErrLimit
			}
		}
	}
	fields, err := objectFields(canonical, []string{"selectedIds"}, "custom", "notes")
	if err != nil {
		return nil, ErrAnswerInvalid
	}
	var ids []string
	if json.Unmarshal(fields["selectedIds"], &ids) != nil || ids == nil {
		return nil, ErrAnswerInvalid
	}
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || len(id) > 128 || (i > 0 && ids[i-1] == id) {
			return nil, ErrAnswerInvalid
		}
	}
	for _, key := range []string{"custom", "notes"} {
		if raw := fields[key]; raw != nil {
			if len(raw) > ChoiceTextBytes {
				return nil, ErrLimit
			}
			text, err := stringField(raw)
			if err != nil || strings.TrimSpace(text) == "" {
				return nil, ErrAnswerInvalid
			}
		}
	}
	fields["selectedIds"], _ = json.Marshal(ids)
	encoded, _ := json.Marshal(fields)
	return jsoncanon.Transform(encoded)
}

// Answer resolves IDs against the immutable question. Callers cannot supply values.
func Answer(question, raw json.RawMessage) (json.RawMessage, error) {
	submitted, err := Submission(raw)
	if err != nil {
		return nil, err
	}
	validated, err := Question(question)
	if err != nil {
		return nil, err
	}
	control, err := parseQuestion(validated)
	if err != nil {
		return nil, err
	}
	if control.Type == "text" {
		if _, err := stringField(submitted); err != nil {
			return nil, ErrAnswerInvalid
		}
		return submitted, nil
	}
	fields, err := objectFields(submitted, []string{"selectedIds"}, "custom", "notes")
	if err != nil {
		return nil, ErrAnswerInvalid
	}
	var ids []string
	if json.Unmarshal(fields["selectedIds"], &ids) != nil {
		return nil, ErrAnswerInvalid
	}
	if (!control.Multiple && len(ids) > 1) || (fields["custom"] != nil && !control.AllowCustom) || (fields["notes"] != nil && !control.AllowNotes) {
		return nil, ErrAnswerInvalid
	}
	if len(ids) == 0 && fields["custom"] == nil {
		return nil, ErrAnswerInvalid
	}
	if !control.Multiple && len(ids) > 0 && fields["custom"] != nil {
		return nil, ErrAnswerInvalid
	}
	selected := make([]choiceOption, 0, len(ids))
	wanted := map[string]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	for _, option := range control.Options {
		if wanted[option.ID] {
			selected = append(selected, option)
			delete(wanted, option.ID)
		}
	}
	if len(wanted) != 0 {
		return nil, ErrAnswerInvalid
	}
	resolved := map[string]any{"selected": selected}
	for _, key := range []string{"custom", "notes"} {
		if fields[key] != nil {
			resolved[key] = fields[key]
		}
	}
	encoded, _ := json.Marshal(resolved)
	canonical, err := jsoncanon.Transform(encoded)
	if err != nil {
		return nil, err
	}
	if len(canonical) > AnswerBytes {
		return nil, ErrLimit
	}
	return canonical, nil
}
