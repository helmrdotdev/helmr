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
