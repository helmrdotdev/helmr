package publicid

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"
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
	if len(seen) != 15 {
		t.Fatalf("registered prefix count = %d, want 15", len(seen))
	}
	if _, ok := seen[Actor]; !ok {
		t.Fatal("Actor prefix is not registered")
	}
	if _, ok := seen[ActorRecord]; !ok {
		t.Fatal("ActorRecord prefix is not registered")
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
	if err := ValidateFor(Workspace, id); err == nil {
		t.Fatalf("ValidateFor(Workspace, %q) succeeded, want error", id)
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

func TestActorRecordIDRoundTrip(t *testing.T) {
	recordID := uuid.MustParse("018f0f31-4c7b-7d2a-8f5b-1a2b3c4d5e6f")
	publicID, err := EncodeActorRecord(recordID)
	if err != nil {
		t.Fatal(err)
	}
	if publicID != "arec_aghq6mkmpn6svd23divtytk6n4" {
		t.Fatalf("EncodeActorRecord() = %q", publicID)
	}
	decoded, err := DecodeActorRecord(publicID)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != recordID {
		t.Fatalf("DecodeActorRecord() = %s, want %s", decoded, recordID)
	}
	if err := ValidateFor(ActorRecord, publicID); err != nil {
		t.Fatalf("ValidateFor(ActorRecord) error = %v", err)
	}
}

func TestActorRecordIDRejectsInvalidForms(t *testing.T) {
	recordID := uuid.MustParse("018f0f31-4c7b-7d2a-8f5b-1a2b3c4d5e6f")
	valid, err := EncodeActorRecord(recordID)
	if err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		" " + valid,
		strings.ToUpper(valid),
		Run.String() + strings.TrimPrefix(valid, ActorRecord.String()),
		valid[:len(valid)-1] + "7",
	}
	for _, value := range invalid {
		t.Run(value, func(t *testing.T) {
			if _, err := DecodeActorRecord(value); err == nil {
				t.Fatalf("DecodeActorRecord(%q) succeeded", value)
			}
			if err := ValidateFor(ActorRecord, value); err == nil {
				t.Fatalf("ValidateFor(ActorRecord, %q) succeeded", value)
			}
			if strings.HasPrefix(value, ActorRecord.String()) && Validate(value) == nil {
				t.Fatalf("Validate(%q) succeeded", value)
			}
		})
	}
	if _, err := EncodeActorRecord(uuid.MustParse("018f0f31-4c7b-4d2a-8f5b-1a2b3c4d5e6f")); err == nil {
		t.Fatal("EncodeActorRecord() accepted UUIDv4")
	}
}

func TestRandomGeneratorRejectsActorRecordPrefix(t *testing.T) {
	if _, err := New(ActorRecord); !errors.Is(err, ErrDerivedPrefix) {
		t.Fatalf("New(ActorRecord) error = %v, want ErrDerivedPrefix", err)
	}
}
