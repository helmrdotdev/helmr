package format

import (
	"bytes"
	"testing"
)

func TestJSONLines(t *testing.T) {
	var out bytes.Buffer

	if err := JSONLines(&out, []map[string]string{
		{"id": "run-1"},
		{"id": "run-2"},
	}); err != nil {
		t.Fatalf("JSONLines() error = %v", err)
	}

	want := "{\"id\":\"run-1\"}\n{\"id\":\"run-2\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("JSONLines() = %q, want %q", got, want)
	}
}

func TestJSON(t *testing.T) {
	var out bytes.Buffer

	if err := JSON(&out, map[string]string{"id": "run-1"}); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	want := "{\"id\":\"run-1\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("JSON() = %q, want %q", got, want)
	}
}
