package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestValidateActorAndSessionIdentifiers(t *testing.T) {
	if err := ValidateActorDeclaredID("operator.v1"); err != nil {
		t.Fatalf("ValidateActorDeclaredID() error = %v", err)
	}
	if err := ValidateSessionID(uuid.Must(uuid.NewV7()).String()); err != nil {
		t.Fatalf("ValidateSessionID() error = %v", err)
	}
	if err := ValidateActorKey("thread:東京"); err != nil {
		t.Fatalf("ValidateActorKey() error = %v", err)
	}
}

func TestValidateActorKeyRejectsMutationProneValues(t *testing.T) {
	for _, key := range []string{
		"",
		" leading",
		"trailing\t",
		"\u00a0leading",
		"trailing\u3000",
		"embedded\x00nul",
		strings.Repeat("a", 513),
	} {
		if err := ValidateActorKey(key); err == nil {
			t.Fatalf("ValidateActorKey(%q) succeeded, want error", key)
		}
	}
}

func TestValidateSendSessionInputRequestAcceptsJSONNull(t *testing.T) {
	if err := ValidateSendSessionInputRequest(SendSessionInputRequest{
		Input: json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSendSessionInputRequestRejectsAmbiguousIJSON(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"value":1,"value":2}`),
		json.RawMessage(`"\ud800"`),
		json.RawMessage(`1e999`),
	} {
		if err := ValidateSendSessionInputRequest(SendSessionInputRequest{
			Input: input,
		}); err == nil {
			t.Fatalf("ValidateSendSessionInputRequest(input=%s) succeeded", input)
		}
	}
}

func TestValidateStartActorRequestPreservesOptionalNullInput(t *testing.T) {
	workspaceID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	if err := ValidateStartActorRequest(StartActorRequest{
		Workspace: WorkspaceIDTarget{ID: workspaceID},
		Input:     json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParseDurationMillisecondsUsesExactPublicGrammar(t *testing.T) {
	for raw, want := range map[string]int64{
		"1ms": 1,
		"90s": 90_000,
		"15m": 900_000,
		"2h":  7_200_000,
		"7d":  604_800_000,
	} {
		got, err := ParseDurationMilliseconds(raw, "duration", 1, MaxQueuedRunTTLMilliseconds)
		if err != nil {
			t.Fatalf("ParseDurationMilliseconds(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseDurationMilliseconds(%q) = %d, want %d", raw, got, want)
		}
	}
	for _, raw := range []string{
		"0s", "01s", "+1s", "-1s", " 1s", "1s ", "1.5s", "1h30m", "1", "1ns",
		"999999999999999999999999999999999999d",
	} {
		if _, err := ParseDurationMilliseconds(raw, "duration", 1, MaxQueuedRunTTLMilliseconds); err == nil {
			t.Fatalf("ParseDurationMilliseconds(%q) succeeded", raw)
		}
	}
}

func TestNormalizeStartActorRetryFillsPublicDefaults(t *testing.T) {
	maxAttempts := int64(3)
	got, err := NormalizeStartActorRetry(&StartActorRetryPolicy{MaxAttempts: &maxAttempts})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1000,"maxMs":30000,"factor":2,"jitter":"full"}}`
	if string(got) != want {
		t.Fatalf("normalized retry = %s, want %s", got, want)
	}
	disabled := false
	got, err = NormalizeStartActorRetry(&StartActorRetryPolicy{Enabled: &disabled})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"enabled":false}` {
		t.Fatalf("disabled retry = %s", got)
	}
}

func TestValidateStartActorRequestRejectsInvalidWorkspaceAndRetry(t *testing.T) {
	workspaceID := "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32"
	maxAttempts := int64(3)
	emptyConcurrencyKey := ""
	for _, request := range []StartActorRequest{
		{},
		{Workspace: WorkspaceIDTarget{ID: "thread:1"}},
		{
			Workspace: WorkspaceIDTarget{ID: workspaceID},
			Run: &StartActorRunOptions{
				TTL:   "1h30m",
				Retry: &StartActorRetryPolicy{MaxAttempts: &maxAttempts},
			},
		},
		{
			Workspace: WorkspaceIDTarget{ID: workspaceID},
			Run: &StartActorRunOptions{
				ConcurrencyKey: &emptyConcurrencyKey,
			},
		},
		{
			Workspace: WorkspaceIDTarget{ID: workspaceID},
			Run: &StartActorRunOptions{
				Retry: &StartActorRetryPolicy{
					MaxAttempts: &maxAttempts,
					Backoff:     &StartActorRetryBackoff{MinDelay: "31s", MaxDelay: "30s"},
				},
			},
		},
	} {
		if err := ValidateStartActorRequest(request); err == nil {
			t.Fatalf("ValidateStartActorRequest(%+v) succeeded", request)
		}
	}
}

func TestValidateCloseSessionRequestAcceptsPathAddressedCommand(t *testing.T) {
	if err := ValidateCloseSessionRequest(CloseSessionRequest{IdempotencyKey: "close-1"}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateActorReadContractUsesClosedEnumsAndReferences(t *testing.T) {
	for _, status := range []SessionStatus{
		SessionStatusOpen,
		SessionStatusClosed,
		SessionStatusCancelled,
		SessionStatusFailed,
	} {
		if err := ValidateSessionStatus(string(status)); err != nil {
			t.Fatalf("ValidateSessionStatus(%q): %v", status, err)
		}
	}
	for _, status := range []string{"", "OPEN", "closing", "unknown"} {
		if err := ValidateSessionStatus(status); err == nil {
			t.Fatalf("ValidateSessionStatus(%q) succeeded", status)
		}
	}
}
