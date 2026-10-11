package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/conversation"
)

var choiceQuestion = json.RawMessage(`{"prompt":[{"type":"text","text":"Pick <@U1> literally"},{"type":"json","value":{"detail":1}}],"answer":{"type":"choice","multiple":true,"allowText":true,"options":[{"id":"a","label":"First","description":"Full first description","value":{"approved":true}},{"id":"b","label":"Second","value":17}]}}`)

func stateFixture(t *testing.T, raw string) formState {
	t.Helper()
	var state formState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSlackFormMapsFrozenValuesInDeclarationOrder(t *testing.T) {
	state := stateFixture(t, `{"choice":{"choice":{"type":"multi_static_select","selected_options":[{"value":"b"},{"value":"a","text":{"text":"forged"}}]}},"text":{"text":{"type":"plain_text_input","value":"note"}}}`)
	answer, err := parseFormAnswer(choiceQuestion, state)
	if err != nil || string(answer) != `{"selected":[{"id":"a","value":{"approved":true}},{"id":"b","value":17}],"text":"note"}` {
		t.Fatalf("wrong answer %s: %v", answer, err)
	}
	for _, choices := range []string{`[{"value":"a"},{"value":"a"}]`, `[{"value":"unknown"}]`} {
		state["choice"]["choice"] = json.RawMessage(`{"type":"multi_static_select","selected_options":` + choices + `}`)
		if _, err := parseFormAnswer(choiceQuestion, state); !errors.Is(err, conversation.ErrAnswerInvalid) {
			t.Fatalf("invalid selection admitted: %v", err)
		}
	}
}

func TestSlackFormRequiresCompleteDeclaredInputs(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}}}`,
		`{"choice":{"choice":{"type":"multi_static_select"}},"text":{"text":{"type":"plain_text_input","value":"note"}}}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}},"text":{"text":{"type":"plain_text_input"}}}`,
		`{"choice":{"choice":{"type":"static_select","selected_option":{"value":"a"}}},"text":{"text":{"type":"plain_text_input","value":null}}}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}},"text":{"text":{"type":"plain_text_input","value":null}}}`,
	} {
		if _, err := parseFormAnswer(choiceQuestion, stateFixture(t, raw)); !errors.Is(err, conversation.ErrAnswerInvalid) {
			t.Fatalf("partial/invalid state accepted %s: %v", raw, err)
		}
	}
	state := stateFixture(t, `{"choice":{"choice":{"type":"multi_static_select","selected_options":null}},"text":{"text":{"type":"plain_text_input","value":"custom"}}}`)
	answer, err := parseFormAnswer(choiceQuestion, state)
	if err != nil || string(answer) != `{"selected":[],"text":"custom"}` {
		t.Fatalf("custom text lost: %s %v", answer, err)
	}
}

func TestSlackFormTextAndSingleChoice(t *testing.T) {
	q := json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`)
	answer, err := parseFormAnswer(q, stateFixture(t, `{"text":{"text":{"type":"plain_text_input","value":null}}}`))
	if err != nil || string(answer) != `""` {
		t.Fatalf("empty text %s %v", answer, err)
	}
	q = json.RawMessage(`{"prompt":[],"answer":{"type":"choice","options":[{"id":"a","label":"A","value":null}]}}`)
	answer, err = parseFormAnswer(q, stateFixture(t, `{"choice":{"choice":{"type":"static_select","selected_option":{"value":"a"}}}}`))
	if err != nil || string(answer) != `{"selected":[{"id":"a","value":null}]}` {
		t.Fatalf("single choice %s %v", answer, err)
	}
}

func TestSlackFormPreservesPromptAndDescriptionsOrRequiresCLI(t *testing.T) {
	view, err := answerModal(choiceQuestion, "signed-form")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Blocks   []json.RawMessage `json:"blocks"`
		Metadata string            `json:"private_metadata"`
		Submit   struct {
			Text string `json:"text"`
		} `json:"submit"`
	}
	if err := json.Unmarshal(view, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Metadata != "signed-form" || decoded.Submit.Text != "Submit answer" || len(decoded.Blocks) != 3 || !strings.Contains(string(view), "Full first description") || !strings.Contains(string(view), "multi_static_select") {
		t.Fatalf("incomplete modal: %s", view)
	}
	var question map[string]any
	json.Unmarshal(choiceQuestion, &question)
	control := question["answer"].(map[string]any)
	options := control["options"].([]any)
	options[0].(map[string]any)["description"] = strings.Repeat("d", 76)
	tooLong, _ := json.Marshal(question)
	if _, err := answerModal(tooLong, "signed-form"); !errors.Is(err, errCLIAnswer) {
		t.Fatalf("description truncated: %v", err)
	}
}

func TestSlackFormFallsBackWhenEncodedViewExceedsNativeSize(t *testing.T) {
	question, _ := json.Marshal(map[string]any{"prompt": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 245000)}}, "answer": map[string]any{"type": "text"}})
	if _, err := conversation.Question(question); err != nil {
		t.Fatal(err)
	}
	if _, err := answerModal(question, "signed-form"); !errors.Is(err, errCLIAnswer) {
		t.Fatalf("oversized native view: %v", err)
	}
}
