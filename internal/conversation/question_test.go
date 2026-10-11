package conversation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func choiceQuestion(multiple, allowText bool, value string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"prompt": []any{map[string]string{"type": "text", "text": "choose\x00雪"}}, "answer": map[string]any{"type": "choice", "multiple": multiple, "allowText": allowText, "options": []any{map[string]any{"id": "one", "label": "First", "value": json.RawMessage(value)}, map[string]any{"id": "two", "label": "Second", "value": false}}}})
	return raw
}
func TestQuestionAnswerContract(t *testing.T) {
	question := choiceQuestion(true, true, `{"z":1,"a":null}`)
	if _, err := Question(question); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		answer string
		valid  bool
	}{
		{`{"selected":[{"id":"one","value":{"a":null,"z":1.0}}]}`, true},
		{`{"selected":[{"id":"one","value":{"a":null,"z":1}},{"id":"two","value":false}],"text":"feedback"}`, true},
		{`{"selected":[],"text":"Allow"}`, true},
		{`{"selected":[],"text":""}`, false},
		{`{"selected":null,"text":"yes"}`, false},
		{`{"selected":[{"id":"two","value":false},{"id":"one","value":{"a":null,"z":1}}]}`, false},
		{`{"selected":[{"id":"two","value":false},{"id":"two","value":false}]}`, false},
		{`{"selected":[{"id":"two","value":true}]}`, false},
		{`{"selected":[{"id":"unknown","value":false}]}`, false},
		{`{"selected":[{"id":"two"}]}`, false},
		{`{"selected":[{"id":"two","value":false}],"respondedBy":{"kind":"user","id":"fake"}}`, false},
	} {
		_, err := Answer(question, json.RawMessage(tc.answer))
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrAnswerInvalid) {
			t.Fatalf("answer %s: %v", tc.answer, err)
		}
	}
	for _, tc := range []struct {
		control, answer string
		valid           bool
	}{
		{`{"type":"text"}`, `""`, true}, {`{"type":"text"}`, `null`, false},
	} {
		q := json.RawMessage(`{"prompt":[],"answer":` + tc.control + `}`)
		_, err := Answer(q, json.RawMessage(tc.answer))
		if tc.valid && err != nil || !tc.valid && !errors.Is(err, ErrAnswerInvalid) {
			t.Fatalf("scalar answer: %v", err)
		}
	}
	single := choiceQuestion(false, false, `null`)
	if _, err := Answer(single, json.RawMessage(`{"selected":[{"id":"one","value":null},{"id":"two","value":false}]}`)); !errors.Is(err, ErrAnswerInvalid) {
		t.Fatalf("multiple single choice: %v", err)
	}
	if _, err := Answer(single, json.RawMessage(`{"selected":[{"id":"two","value":false}],"text":"feedback"}`)); !errors.Is(err, ErrAnswerInvalid) {
		t.Fatalf("forbidden text: %v", err)
	}
}
func TestQuestionBoundsGuaranteeAnswerability(t *testing.T) {
	answer := func(text string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"selected": []any{}, "text": text})
		return raw
	}
	question := choiceQuestion(false, true, `null`)
	if _, err := Answer(question, answer(strings.Repeat("x", ChoiceTextBytes-2))); err != nil {
		t.Fatal(err)
	}
	if _, err := Answer(question, answer(strings.Repeat("x", ChoiceTextBytes-1))); !errors.Is(err, ErrLimit) {
		t.Fatalf("custom text bound: %v", err)
	}
	// One enormous frozen option cannot ever be answered, even if its question fits.
	large, _ := json.Marshal(strings.Repeat("x", AnswerBytes))
	if _, err := Question(choiceQuestion(false, false, string(large))); !errors.Is(err, ErrLimit) {
		t.Fatalf("unanswerable option: %v", err)
	}
	// Reserve custom text in addition to selection bytes at admission.
	near, _ := json.Marshal(strings.Repeat("x", AnswerBytes-4096))
	if _, err := Question(choiceQuestion(false, false, string(near))); err != nil {
		t.Fatal(err)
	}
	if _, err := Question(choiceQuestion(false, true, string(near))); !errors.Is(err, ErrLimit) {
		t.Fatalf("missing text reservation: %v", err)
	}
	raw, _ := json.Marshal(strings.Repeat("x", AnswerBytes-2))
	q := json.RawMessage(`{"prompt":[],"answer":{"type":"text"}}`)
	if _, err := Answer(q, raw); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(strings.Repeat("x", AnswerBytes-1))
	if _, err := Answer(q, raw); !errors.Is(err, ErrLimit) {
		t.Fatalf("answer byte bound: %v", err)
	}
	for _, control := range []string{`{"type":"text","options":[]}`, `{"type":"unknown"}`, `{"type":"choice","options":[]}`, `{"type":"choice","options":[{"id":"a","label":"A","value":null}],"allowText":null}`} {
		if _, err := Question(json.RawMessage(`{"prompt":[],"answer":` + control + `}`)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid control %s: %v", control, err)
		}
	}
}

func TestChoiceAnswerSizePrecedesShape(t *testing.T) {
	for _, shape := range []map[string]any{
		{"selected": nil}, {}, {"selected": []any{}, "extra": true},
		{"selected": []any{map[string]any{"id": "one", "value": nil}, map[string]any{"id": "two", "value": false}}},
	} {
		shape["text"] = strings.Repeat("x", ChoiceTextBytes-1)
		raw, _ := json.Marshal(shape)
		if _, err := Answer(choiceQuestion(false, false, `null`), raw); !errors.Is(err, ErrLimit) {
			t.Fatalf("size precedence: %v", err)
		}
	}
}

func TestQuestionExactCombinedAnswerBoundary(t *testing.T) {
	text := strings.Repeat("x", ChoiceTextBytes-2)
	base, _ := json.Marshal(map[string]any{"selected": []any{map[string]any{"id": "one", "value": ""}}, "text": text})
	value := strings.Repeat("x", AnswerBytes-len(base))
	question := func(v string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"prompt": []any{}, "answer": map[string]any{"type": "choice", "allowText": true, "options": []any{map[string]any{"id": "one", "label": "One", "value": v}}}})
		return raw
	}
	q := question(value)
	if _, err := Question(q); err != nil {
		t.Fatal(err)
	}
	answer, _ := json.Marshal(map[string]any{"selected": []any{map[string]any{"id": "one", "value": value}}, "text": text})
	if _, err := Answer(q, answer); err != nil {
		t.Fatal(err)
	}
	if _, err := Question(question(value + "x")); !errors.Is(err, ErrLimit) {
		t.Fatalf("combined overflow: %v", err)
	}
}
