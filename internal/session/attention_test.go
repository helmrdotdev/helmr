package session

import (
	"strings"
	"testing"
)

func TestQuestionSummaryIsTheFirstTextPartOnOneShortLine(t *testing.T) {
	long := strings.Repeat("雪", attentionSummaryRunes+5)
	for _, tc := range []struct {
		name, question, want string
	}{
		{"first text part", `{"answer":{"type":"text"},"prompt":[{"type":"json","value":{"a":1}},{"type":"text","text":"Ship\n\tthe  release?\u0000"},{"type":"text","text":"later"}]}`, "Ship the release?"},
		{"no text part", `{"answer":{"type":"text"},"prompt":[{"type":"json","value":1}]}`, ""},
		{"shortened", `{"answer":{"type":"text"},"prompt":[{"type":"text","text":"` + long + `"}]}`, strings.Repeat("雪", attentionSummaryRunes) + "…"},
		{"expired", "", ""},
		{"unreadable", `{"prompt":`, ""},
	} {
		var raw []byte
		if tc.question != "" {
			raw = []byte(tc.question)
		}
		if got := questionSummary(raw); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
