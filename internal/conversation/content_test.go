package conversation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestContentCanonicalBoundaries(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		shorthand   bool
	}{
		{`""`, `[{"text":"","type":"text"}]`, true},
		{`[]`, `[]`, false},
		{`[{"type":"text","text":"a\u0000日"},{"type":"text","text":"b"},{"type":"json","value":{"z":1.0,"a":false}}]`, `[{"text":"a\u0000日","type":"text"},{"text":"b","type":"text"},{"type":"json","value":{"a":false,"z":1}}]`, false},
	} {
		got, err := Content(json.RawMessage(tc.input), tc.shorthand)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%s -> %s: %v", tc.input, got, err)
		}
	}
	empty, _ := Content(json.RawMessage(`""`), true)
	for _, extra := range []int{0, 1} {
		raw, _ := json.Marshal(strings.Repeat("x", ContentBytes-len(empty)+extra))
		got, err := Content(raw, true)
		if extra == 0 && (err != nil || len(got) != ContentBytes) {
			t.Fatalf("exact boundary %d %v", len(got), err)
		}
		if extra == 1 && !errors.Is(err, ErrLimit) {
			t.Fatalf("oversize %v", err)
		}
	}
	for _, tc := range []struct {
		input string
		want  error
	}{
		{`null`, ErrInvalid}, {`"text"`, ErrInvalid}, {`[{"type":"text","text":null}]`, ErrInvalid},
		{`[{"type":"text","text":"a","text":"b"}]`, ErrInvalid},
		{`[{"type":"text","text":"\ud800"}]`, ErrInvalid},
		{`[{"type":"json","value":1e999}]`, ErrInvalid},
		{`[{"type":"file","file":"anything"}]`, ErrUnsupported},
		{`[` + strings.Repeat(`{"type":"json","value":null},`, 64) + `{"type":"json","value":null}]`, ErrLimit},
	} {
		if _, err := Content(json.RawMessage(tc.input), false); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.input, err)
		}
	}
}
