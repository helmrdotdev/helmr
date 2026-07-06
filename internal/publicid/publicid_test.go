package publicid

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestNewGeneratesRegisteredPrefixedID(t *testing.T) {
	reader := bytes.NewReader([]byte{
		0x00, 0x11, 0x22, 0x33,
		0x44, 0x55, 0x66, 0x77,
		0x88, 0x99, 0xaa, 0xbb,
		0xcc, 0xdd, 0xee, 0xff,
	})

	id, err := NewWithReader(Run, reader)
	if err != nil {
		t.Fatalf("NewWithReader() error = %v", err)
	}
	if !strings.HasPrefix(id, Run.String()) {
		t.Fatalf("id %q does not start with %q", id, Run)
	}
	if len(id) != len(Run.String())+randomLength {
		t.Fatalf("id length = %d, want %d", len(id), len(Run.String())+randomLength)
	}
	if strings.ToLower(id) != id {
		t.Fatalf("id %q is not lowercase", id)
	}
	if err := ValidateFor(Run, id); err != nil {
		t.Fatalf("ValidateFor() error = %v", err)
	}
}

func TestRegisteredPrefixesAreValidAndUnambiguous(t *testing.T) {
	seen := map[Prefix]struct{}{}
	for _, prefix := range RegisteredPrefixes() {
		if !prefix.Valid() {
			t.Fatalf("prefix %q is not valid", prefix)
		}
		if _, ok := seen[prefix]; ok {
			t.Fatalf("duplicate prefix %q", prefix)
		}
		seen[prefix] = struct{}{}
		if !strings.HasSuffix(prefix.String(), "_") {
			t.Fatalf("prefix %q must include trailing underscore", prefix)
		}
	}
	if len(seen) != 22 {
		t.Fatalf("registered prefix count = %d, want 22", len(seen))
	}
}

func TestParseRejectsInvalidIDs(t *testing.T) {
	tests := []string{
		"",
		"run",
		"run_",
		"RUN_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"unknown_aaaaaaaaaaaaaaaaaaaaaaaaaa",
		"run_aaaaaaaaaaaaaaaaaaaaaaaaa",
		"run_aaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"run_aaaaaaaaaaaaaaaaaaaaaaaaa0",
		"run_aaaaaaaaaaaaaaaaaaaaaaaaa8",
		"run_aaaaaaaaaaaaaaaaaaaaaaaaa-",
	}

	for _, id := range tests {
		t.Run(id, func(t *testing.T) {
			if err := Validate(id); err == nil {
				t.Fatalf("Validate(%q) succeeded, want error", id)
			}
		})
	}
}

func TestValidateForRejectsWrongPrefix(t *testing.T) {
	id := "run_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ValidateFor(Session, id); err == nil {
		t.Fatalf("ValidateFor(Session, %q) succeeded, want error", id)
	}
}

func TestRegexpMatchesGeneratedIDs(t *testing.T) {
	pattern, err := Run.Regexp()
	if err != nil {
		t.Fatalf("Regexp() error = %v", err)
	}
	if pattern != `^run_[a-z2-7]{26}$` {
		t.Fatalf("pattern = %q", pattern)
	}
}

func TestNewWithReaderReturnsEntropyError(t *testing.T) {
	_, err := NewWithReader(Run, io.LimitReader(bytes.NewReader(nil), 0))
	if err == nil {
		t.Fatal("NewWithReader() succeeded, want error")
	}
}
