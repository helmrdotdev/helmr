package content

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func choiceQuestion(multiple, custom bool, value string) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{"prompt": []any{map[string]string{"type": "text", "text": "choose\x00雪"}}, "type": "choice", "multiple": multiple, "allowCustom": custom, "allowNotes": true, "options": []any{map[string]any{"id": "one", "label": "First", "value": json.RawMessage(value)}, map[string]any{"id": "two", "label": "Second", "value": false}}})
	return raw
}
func TestQuestionAnswerContract(t *testing.T) {
	q := choiceQuestion(true, true, `{"z":1,"a":null}`)
	for _, tc := range []struct{ input, want string }{
		{`{"selectedIds":["one"]}`, `{"selected":[{"id":"one","value":{"a":null,"z":1}}]}`},
		{`{"selectedIds":["two","one"],"custom":"another","notes":"context"}`, `{"custom":"another","notes":"context","selected":[{"id":"one","value":{"a":null,"z":1}},{"id":"two","value":false}]}`},
		{`{"selectedIds":[],"custom":"Allow"}`, `{"custom":"Allow","selected":[]}`},
	} {
		got, err := Answer(q, json.RawMessage(tc.input))
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s: %s %v", tc.input, got, err)
		}
	}
	for _, raw := range []string{`{"selectedIds":[],"notes":"alone"}`, `{"selectedIds":[],"custom":" "}`, `{"selectedIds":null}`, `{"selectedIds":["two","two"]}`, `{"selectedIds":["unknown"]}`, `{"selectedIds":["two"],"answeredBy":"fake"}`, `{"selected":[{"id":"two","value":false}]}`, `{"selectedIds":["two"],"value":true}`} {
		if _, err := Answer(q, json.RawMessage(raw)); !errors.Is(err, ErrAnswerInvalid) {
			t.Fatalf("accepted invalid %s: %v", raw, err)
		}
	}
	single := choiceQuestion(false, true, `null`)
	for _, raw := range []string{`{"selectedIds":["one","two"]}`, `{"selectedIds":["one"],"custom":"alternative"}`} {
		if _, err := Answer(single, json.RawMessage(raw)); !errors.Is(err, ErrAnswerInvalid) {
			t.Fatal(raw, err)
		}
	}
	if _, err := Answer(single, []byte(`{"selectedIds":["one"],"notes":"explanation"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`""`, `"answer"`} {
		if _, err := Answer([]byte(`{"prompt":[],"type":"text"}`), []byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Answer([]byte(`{"prompt":[],"type":"text"}`), []byte(`null`)); !errors.Is(err, ErrAnswerInvalid) {
		t.Fatal(err)
	}
}
func TestQuestionBoundsGuaranteeAnswerability(t *testing.T) {
	q := choiceQuestion(true, true, `null`)
	for _, n := range []int{ChoiceTextBytes - 2, ChoiceTextBytes - 1} {
		raw, _ := json.Marshal(map[string]any{"selectedIds": []string{}, "custom": strings.Repeat("x", n)})
		_, err := Answer(q, raw)
		if (n == ChoiceTextBytes-2 && err != nil) || (n == ChoiceTextBytes-1 && !errors.Is(err, ErrLimit)) {
			t.Fatal(n, err)
		}
	}
	large, _ := json.Marshal(strings.Repeat("x", AnswerBytes))
	if _, err := Question(choiceQuestion(false, false, string(large))); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"prompt":[],"type":"text","options":[]}`, `{"prompt":[],"type":"unknown"}`, `{"prompt":[],"type":"choice","options":[]}`, `{"prompt":[],"type":"text","title":""}`} {
		if _, err := Question([]byte(raw)); !errors.Is(err, ErrInvalid) {
			t.Fatal(raw, err)
		}
	}
	raw, _ := json.Marshal(strings.Repeat("x", AnswerBytes-2))
	if _, err := Answer([]byte(`{"prompt":[],"type":"text"}`), raw); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(strings.Repeat("x", AnswerBytes-1))
	if _, err := Answer([]byte(`{"prompt":[],"type":"text"}`), raw); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestQuestionExactCombinedAnswerBoundary(t *testing.T) {
	notes := strings.Repeat("x", ChoiceTextBytes-2)
	base, _ := json.Marshal(map[string]any{"selected": []any{map[string]any{"id": "one", "value": ""}}, "notes": notes})
	value := strings.Repeat("x", AnswerBytes-len(base))
	question := func(v string) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"prompt": []any{}, "type": "choice", "allowNotes": true, "options": []any{map[string]any{"id": "one", "label": "One", "value": v}}})
		return raw
	}
	q := question(value)
	if _, err := Question(q); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"selectedIds": []string{"one"}, "notes": notes})
	got, err := Answer(q, raw)
	if err != nil || len(got) != AnswerBytes {
		t.Fatal(len(got), err)
	}
	if _, err := Question(question(value + "x")); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
func TestSubmissionIdentityIgnoresChoiceOrder(t *testing.T) {
	a, err := Submission([]byte(`{"selectedIds":["two","one"],"notes":"keep"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Submission([]byte(`{"notes":"keep","selectedIds":["one","two"]}`))
	if err != nil || string(a) != string(b) {
		t.Fatal(string(a), string(b), err)
	}
}
