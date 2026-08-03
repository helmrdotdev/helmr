package main

import (
	"bytes"
	"testing"
)

func TestWriteJSONLines(t *testing.T) {
	var out bytes.Buffer

	if err := writeJSONLines(&out, []map[string]string{
		{"id": "run-1"},
		{"id": "run-2"},
	}); err != nil {
		t.Fatalf("writeJSONLines() error = %v", err)
	}

	want := "{\"id\":\"run-1\"}\n{\"id\":\"run-2\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("writeJSONLines() = %q, want %q", got, want)
	}
}

func TestWriteJSON(t *testing.T) {
	var out bytes.Buffer

	if err := writeJSON(&out, map[string]string{"id": "run-1"}); err != nil {
		t.Fatalf("writeJSON() error = %v", err)
	}

	want := "{\"id\":\"run-1\"}\n"
	if got := out.String(); got != want {
		t.Fatalf("writeJSON() = %q, want %q", got, want)
	}
}
