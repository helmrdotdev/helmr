package conversation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestUniformTextInput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		want        error
	}{
		{"text Unicode", `[{"type":"text","text":"a\u0000雪"}]`, nil},
		{"empty content", `[]`, nil},
		{"object", `{"revision":"abc"}`, ErrInvalid},
		{"null", `null`, ErrInvalid},
		{"string shorthand", `"hello"`, ErrInvalid},
		{"old wrapper", `{"type":"message","content":[]}`, ErrInvalid},
		{"json part", `[{"type":"json","value":null}]`, ErrUnsupported},
		{"file", `[{"type":"file","id":"x"}]`, ErrUnsupported},
		{"extra context", `[{"type":"text","text":"a","actor":"x"}]`, ErrInvalid},
		{"duplicate keys", `[{"type":"text","text":"a","text":"b"}]`, ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Input(json.RawMessage(tc.input))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
	tooLarge, _ := json.Marshal([]map[string]string{{"type": "text", "text": strings.Repeat("x", ContentBytes)}})
	if _, err := Input(tooLarge); !errors.Is(err, ErrLimit) {
		t.Fatal("oversized input accepted", err)
	}
	raw := json.RawMessage(`[{"type":"text","text":"Hello"},{"type":"text","text":" world\n"},{"type":"text","text":"  {\"a\":1}\n"}]`)
	if text, err := InputText(raw); err != nil || text != "Hello world\n  {\"a\":1}\n" {
		t.Fatalf("projection changed text %q: %v", text, err)
	}
}
