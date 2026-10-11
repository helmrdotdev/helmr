package api

import (
	"encoding/json"
	"testing"
)

func TestValidateSessionDataRequestAcceptsJSONNull(t *testing.T) {
	if err := ValidateSessionDataRequest(SessionDataRequest{
		Data: json.RawMessage(`null`),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestValidateSessionDataRequestRejectsAmbiguousIJSON(t *testing.T) {
	for _, input := range []json.RawMessage{
		json.RawMessage(`{"value":1,"value":2}`),
		json.RawMessage(`"\ud800"`),
		json.RawMessage(`1e999`),
	} {
		if err := ValidateSessionDataRequest(SessionDataRequest{
			Data: input,
		}); err == nil {
			t.Fatalf("ValidateSessionDataRequest(input=%s) succeeded", input)
		}
	}
}
