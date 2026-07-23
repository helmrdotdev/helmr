package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateActorReferenceParts(t *testing.T) {
	if err := ValidateActorDeclaredID("operator.v1"); err != nil {
		t.Fatalf("ValidateActorDeclaredID() error = %v", err)
	}
	if err := ValidateActorPublicID("act_aaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatalf("ValidateActorPublicID() error = %v", err)
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

func TestValidateSendActorInputRequestAcceptsJSONNull(t *testing.T) {
	if err := ValidateSendActorInputRequest(SendActorInputRequest{
		ActorKey: "thread:1",
		Input:    json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSendActorInputRequestRejectsAmbiguousIJSON(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"value":1,"value":2}`),
		json.RawMessage(`"\ud800"`),
		json.RawMessage(`1e999`),
	} {
		if err := ValidateSendActorInputRequest(SendActorInputRequest{
			ActorKey: "thread:1",
			Input:    input,
		}); err == nil {
			t.Fatalf("ValidateSendActorInputRequest(input=%s) succeeded", input)
		}
	}
}

func TestValidateStartActorRequestPreservesOptionalNullInput(t *testing.T) {
	workspaceID := "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := ValidateStartActorRequest(StartActorRequest{
		Workspace: StartActorWorkspaceTarget{ID: &workspaceID},
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

func TestParseRFC3339NanosecondInstantUsesExactPublicGrammar(t *testing.T) {
	for _, raw := range []string{
		"2028-01-02T03:04:05Z",
		"2028-01-02T03:04:05.123456789+09:00",
		"2028-01-02T03:04:05-23:59",
	} {
		got, err := ParseRFC3339NanosecondInstant(raw)
		if err != nil {
			t.Fatalf("ParseRFC3339NanosecondInstant(%q): %v", raw, err)
		}
		if got.Location() == nil {
			t.Fatalf("ParseRFC3339NanosecondInstant(%q) has no location", raw)
		}
	}
	for _, raw := range []string{
		"",
		"2028-01-02",
		"2028-01-02T03:04:05",
		"2028-01-02t03:04:05z",
		"2028-01-02T03:04:05,1Z",
		"2028-01-02T03:04:05.1234567890Z",
		"2028-13-02T03:04:05Z",
		"2028-01-02T03:04:05+24:00",
		"2028-01-02T03:04:05+23:60",
		"2028-01-02T03:04:05-24:00",
	} {
		if _, err := ParseRFC3339NanosecondInstant(raw); err == nil {
			t.Fatalf("ParseRFC3339NanosecondInstant(%q) succeeded", raw)
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
	workspaceID := "wsp_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	workspaceKey := "thread:1"
	maxAttempts := int64(3)
	emptyConcurrencyKey := ""
	for _, request := range []StartActorRequest{
		{},
		{Workspace: StartActorWorkspaceTarget{ID: &workspaceID, Key: &workspaceKey}},
		{
			Workspace: StartActorWorkspaceTarget{ID: &workspaceID},
			Run: &StartActorRunOptions{
				TTL:   "1h30m",
				Retry: &StartActorRetryPolicy{MaxAttempts: &maxAttempts},
			},
		},
		{
			Workspace: StartActorWorkspaceTarget{ID: &workspaceID},
			Run: &StartActorRunOptions{
				ConcurrencyKey: &emptyConcurrencyKey,
			},
		},
		{
			Workspace: StartActorWorkspaceTarget{ID: &workspaceID},
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

func TestValidateActorOperationRequestRequiresOneExactAddress(t *testing.T) {
	validID := "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, request := range []ActorOperationRequest{
		{ActorID: validID},
		{ActorKey: "thread:1"},
	} {
		if err := ValidateActorOperationRequest(request); err != nil {
			t.Fatalf("ValidateActorOperationRequest(%+v): %v", request, err)
		}
	}
	for _, request := range []ActorOperationRequest{
		{},
		{ActorID: validID, ActorKey: "thread:1"},
		{ActorID: "act_invalid"},
		{ActorKey: " thread:1"},
	} {
		if err := ValidateActorOperationRequest(request); err == nil {
			t.Fatalf("ValidateActorOperationRequest(%+v) succeeded", request)
		}
	}
}

func TestValidateActorReadContractUsesClosedEnumsAndReferences(t *testing.T) {
	for _, lifecycle := range []ActorLifecycle{
		ActorLifecycleOpen,
		ActorLifecycleClosing,
		ActorLifecycleClosed,
		ActorLifecycleCancelling,
		ActorLifecycleCancelled,
		ActorLifecycleFailed,
		ActorLifecycleExpired,
	} {
		if err := ValidateActorLifecycle(string(lifecycle)); err != nil {
			t.Fatalf("ValidateActorLifecycle(%q): %v", lifecycle, err)
		}
	}
	for _, lifecycle := range []string{"", "OPEN", "unknown"} {
		if err := ValidateActorLifecycle(lifecycle); err == nil {
			t.Fatalf("ValidateActorLifecycle(%q) succeeded", lifecycle)
		}
	}
	validID := "act_aaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, reference := range []ActorReference{
		{ActorID: validID},
		{ActorKey: "thread:1"},
	} {
		if err := ValidateActorReference(reference); err != nil {
			t.Fatalf("ValidateActorReference(%+v): %v", reference, err)
		}
	}
	for _, reference := range []ActorReference{
		{},
		{ActorID: validID, ActorKey: "thread:1"},
		{ActorID: "act_invalid"},
		{ActorKey: " thread:1"},
	} {
		if err := ValidateActorReference(reference); err == nil {
			t.Fatalf("ValidateActorReference(%+v) succeeded", reference)
		}
	}
}
