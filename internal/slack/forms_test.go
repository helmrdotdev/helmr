package slack

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/content"
)

var choiceQuestion = json.RawMessage(`{"prompt":[{"type":"text","text":"Pick <@U1> literally"},{"type":"json","value":{"detail":1}}],"type":"choice","multiple":true,"allowCustom":true,"options":[{"id":"a","label":"First","description":"Full first description","value":{"approved":true}},{"id":"b","label":"Second","value":17}]}`)

func stateFixture(t *testing.T, raw string) formState {
	t.Helper()
	var state formState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSlackFormMapsFrozenValuesInDeclarationOrder(t *testing.T) {
	state := stateFixture(t, `{"choice":{"choice":{"type":"multi_static_select","selected_options":[{"value":"b"},{"value":"a","custom":{"custom":"forged"}}]}},"custom":{"custom":{"type":"plain_text_input","value":"note"}}}`)
	answer, err := parseFormAnswer(choiceQuestion, state)
	if err != nil || string(answer) != `{"custom":"note","selectedIds":["a","b"]}` {
		t.Fatalf("wrong answer %s: %v", answer, err)
	}
	for _, choices := range []string{`[{"value":"a"},{"value":"a"}]`, `[{"value":"unknown"}]`} {
		state["choice"]["choice"] = json.RawMessage(`{"type":"multi_static_select","selected_options":` + choices + `}`)
		if _, err := parseFormAnswer(choiceQuestion, state); !errors.Is(err, content.ErrAnswerInvalid) {
			t.Fatalf("invalid selection admitted: %v", err)
		}
	}
}

func TestSlackFormRequiresCompleteDeclaredInputs(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}}}`,
		`{"choice":{"choice":{"type":"multi_static_select"}},"custom":{"custom":{"type":"plain_text_input","value":"note"}}}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}},"custom":{"custom":{"type":"plain_text_input"}}}`,
		`{"choice":{"choice":{"type":"static_select","selected_option":{"value":"a"}}},"custom":{"custom":{"type":"plain_text_input","value":null}}}`,
		`{"choice":{"choice":{"type":"multi_static_select","selected_options":[]}},"custom":{"custom":{"type":"plain_text_input","value":null}}}`,
	} {
		if _, err := parseFormAnswer(choiceQuestion, stateFixture(t, raw)); !errors.Is(err, content.ErrAnswerInvalid) {
			t.Fatalf("partial/invalid state accepted %s: %v", raw, err)
		}
	}
	state := stateFixture(t, `{"choice":{"choice":{"type":"multi_static_select","selected_options":null}},"custom":{"custom":{"type":"plain_text_input","value":"custom"}}}`)
	answer, err := parseFormAnswer(choiceQuestion, state)
	if err != nil || string(answer) != `{"custom":"custom","selectedIds":[]}` {
		t.Fatalf("custom text lost: %s %v", answer, err)
	}
}

func TestSlackFormTextAndSingleChoice(t *testing.T) {
	q := json.RawMessage(`{"prompt":[],"type":"text"}`)
	answer, err := parseFormAnswer(q, stateFixture(t, `{"text":{"text":{"type":"plain_text_input","value":null}}}`))
	if err != nil || string(answer) != `""` {
		t.Fatalf("empty text %s %v", answer, err)
	}
	q = json.RawMessage(`{"prompt":[],"type":"choice","options":[{"id":"a","label":"A","value":null}]}`)
	answer, err = parseFormAnswer(q, stateFixture(t, `{"choice":{"choice":{"type":"static_select","selected_option":{"value":"a"}}}}`))
	if err != nil || string(answer) != `{"selectedIds":["a"]}` {
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
	control := question
	options := control["options"].([]any)
	options[0].(map[string]any)["description"] = strings.Repeat("d", 76)
	tooLong, _ := json.Marshal(question)
	view, err = answerModal(tooLong, "signed-form")
	if err != nil || !strings.Contains(string(view), strings.Repeat("d", 76)) || !strings.Contains(string(view), "multi_static_select") {
		t.Fatalf("long description lost or no answer control: %s %v", view, err)
	}
}

func TestSlackFormFallsBackWhenEncodedViewExceedsNativeSize(t *testing.T) {
	question, _ := json.Marshal(map[string]any{"prompt": []any{map[string]any{"type": "text", "text": strings.Repeat("x", 245000)}}, "type": "text"})
	if _, err := content.Question(question); err != nil {
		t.Fatal(err)
	}
	if _, err := answerModal(question, "signed-form"); !errors.Is(err, errCLIAnswer) {
		t.Fatalf("oversized native view: %v", err)
	}
}

func TestInlineAnswerValidatesSelectionAndDoesNotImportPrivateFields(t *testing.T) {
	q := json.RawMessage(`{"prompt":[],"type":"choice","allowCustom":true,"allowNotes":true,"options":[{"id":"a","label":"A","value":null}]}`)
	for _, raw := range []string{`{}`, `{"choice":{"choice":{"type":"radio_buttons","selected_option":null}}}`, `{"choice":{"choice":{"type":"radio_buttons","selected_option":{"value":"unknown"}}}}`} {
		if _, err := parseInlineAnswer(q, stateFixture(t, raw)); !errors.Is(err, content.ErrAnswerInvalid) {
			t.Fatalf("invalid inline answer accepted: %s %v", raw, err)
		}
	}
	state := stateFixture(t, `{"choice":{"choice":{"type":"radio_buttons","selected_option":{"value":"a"}}},"notes":{"notes":{"type":"plain_text_input","value":"forged"}},"custom":{"custom":{"type":"plain_text_input","value":"forged"}}}`)
	answer, err := parseInlineAnswer(q, state)
	if err != nil || string(answer) != `{"selectedIds":["a"]}` {
		t.Fatal(string(answer), err)
	}
	q = json.RawMessage(`{"prompt":[],"type":"choice","multiple":true,"options":[{"id":"a","label":"A","value":null},{"id":"b","label":"B","value":null}]}`)
	state = stateFixture(t, `{"choice":{"choice":{"type":"checkboxes","selected_options":[{"value":"b"},{"value":"a"}]}}}`)
	answer, err = parseInlineAnswer(q, state)
	if err != nil || string(answer) != `{"selectedIds":["a","b"]}` {
		t.Fatal(string(answer), err)
	}
}

func TestSlackQuestionPreservesTitleInMessageAndModal(t *testing.T) {
	q := json.RawMessage(`{"title":"Delete archived workspace?","prompt":[{"type":"text","text":"This cannot be undone"}],"type":"choice","options":[{"id":"yes","label":"Yes","value":true},{"id":"no","label":"No","value":false}]}`)
	posts, err := renderQuestion(1, q)
	if err != nil || len(posts) != 1 {
		t.Fatalf("question rendering: %v", err)
	}
	blocks, _ := json.Marshal(posts[0].Body.Blocks)
	if !strings.Contains(string(blocks), "Delete archived workspace?") || !strings.Contains(posts[0].Body.Text, "Delete archived workspace?\n\nThis cannot be undone") {
		t.Fatalf("missing confirmation subject: %+v", posts[0])
	}
	view, err := answerModal(q, "signed-form")
	if err != nil || !strings.Contains(string(view), "Delete archived workspace?") || !strings.Contains(string(view), "This cannot be undone") {
		t.Fatalf("incomplete confirmation: %s %v", view, err)
	}
}
