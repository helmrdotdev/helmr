package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const ChoiceTextBytes = 8 * 1024

var ErrAnswerInvalid = errors.New("answer does not match its declared control")

type choiceOption struct {
	ID    string          `json:"id"`
	Value json.RawMessage `json:"value"`
}
type answerControl struct {
	Type      string
	Options   []choiceOption
	Multiple  bool
	AllowText bool
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

// Question validates a complete prompt/control before it can consume budget or
// become visible. In particular every configured choice must be answerable.
func Question(raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(canonical) > ContentBytes {
		return nil, fmt.Errorf("%w: Question exceeds 256 KiB", ErrLimit)
	}
	fields, err := objectFields(canonical, []string{"prompt", "answer"})
	if err != nil {
		return nil, err
	}
	if _, err := Content(fields["prompt"], false); err != nil {
		return nil, err
	}
	if _, err := parseControl(fields["answer"]); err != nil {
		return nil, err
	}
	return canonical, nil
}
func parseControl(raw json.RawMessage) (answerControl, error) {
	fields, err := objectFields(raw, []string{"type"}, "options", "multiple", "allowText")
	if err != nil {
		return answerControl{}, err
	}
	kind, err := stringField(fields["type"])
	if err != nil {
		return answerControl{}, err
	}
	result := answerControl{Type: kind}
	if kind == "text" {
		if len(fields) != 1 {
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
	if result.AllowText, err = boolField(fields["allowText"]); err != nil {
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
	if result.AllowText {
		maxBytes += len(`,"text":`) + ChoiceTextBytes
	}
	if maxBytes > AnswerBytes {
		return result, fmt.Errorf("%w: Configured choice answer exceeds 64 KiB", ErrLimit)
	}
	return result, nil
}

// Answer preserves the submitted JSON exactly after canonicalization; option
// values cannot be replaced, expanded from IDs, reordered or interpreted here.
func Answer(question, raw json.RawMessage) (json.RawMessage, error) {
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, ErrAnswerInvalid
	}
	if len(canonical) > AnswerBytes {
		return nil, fmt.Errorf("%w: Answer exceeds 64 KiB", ErrLimit)
	}
	validated, err := Question(question)
	if err != nil {
		return nil, err
	}
	fields, _ := objectFields(validated, []string{"prompt", "answer"})
	control, err := parseControl(fields["answer"])
	if err != nil {
		return nil, err
	}
	if control.Type == "text" {
		if _, err := stringField(canonical); err != nil {
			return nil, ErrAnswerInvalid
		}
		return canonical, nil
	}
	// Size errors take precedence over shape errors for a present choice text member.
	var envelope map[string]json.RawMessage
	if json.Unmarshal(canonical, &envelope) == nil && len(envelope["text"]) > ChoiceTextBytes {
		return nil, fmt.Errorf("%w: Choice custom text exceeds 8 KiB", ErrLimit)
	}
	answer, err := objectFields(canonical, []string{"selected"}, "text")
	if err != nil {
		return nil, ErrAnswerInvalid
	}
	var selected []json.RawMessage
	if json.Unmarshal(answer["selected"], &selected) != nil || selected == nil || (!control.Multiple && len(selected) > 1) {
		return nil, ErrAnswerInvalid
	}
	text := ""
	if rawText := answer["text"]; rawText != nil {
		if !control.AllowText {
			return nil, ErrAnswerInvalid
		}
		text, err = stringField(rawText)
		if err != nil {
			return nil, ErrAnswerInvalid
		}
	}
	if len(selected) == 0 && text == "" {
		return nil, ErrAnswerInvalid
	}
	previous := -1
	for _, rawSelection := range selected {
		selection, err := objectFields(rawSelection, []string{"id", "value"})
		if err != nil {
			return nil, ErrAnswerInvalid
		}
		id, err := stringField(selection["id"])
		if err != nil {
			return nil, ErrAnswerInvalid
		}
		index := -1
		for i, option := range control.Options {
			if option.ID == id {
				index = i
				break
			}
		}
		if index <= previous || index < 0 || !bytes.Equal(selection["value"], control.Options[index].Value) {
			return nil, ErrAnswerInvalid
		}
		previous = index
	}
	return canonical, nil
}
