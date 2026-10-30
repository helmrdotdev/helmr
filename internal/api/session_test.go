package api

import (
	"encoding/json"
	"testing"
)

func TestValidateSessionMessageRequestAcceptsJSONNull(t *testing.T) {
	if err := ValidateSessionMessageRequest(SessionMessageRequest{
		Message: json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSessionMessageRequestRejectsAmbiguousIJSON(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"value":1,"value":2}`),
		json.RawMessage(`"\ud800"`),
		json.RawMessage(`1e999`),
	} {
		if err := ValidateSessionMessageRequest(SessionMessageRequest{
			Message: input,
		}); err == nil {
			t.Fatalf("ValidateSessionMessageRequest(input=%s) succeeded", input)
		}
	}
}
