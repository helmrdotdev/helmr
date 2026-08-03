package workerapi

import (
	"encoding/json"
	"testing"
)

func TestWorkerActorOutcomeRequiresPresentCursorAndOneVariant(t *testing.T) {
	for _, raw := range []string{
		`{"succeeded":{}}`,
		`{"terminal_input_sequence":0}`,
		`{"terminal_input_sequence":0,"succeeded":{},"failed":{"message":"x"}}`,
		`{"terminal_input_sequence":-1,"succeeded":{}}`,
		`{"terminal_input_sequence":0,"succeeded":null}`,
		`{"terminal_input_sequence":0,"succeeded":{},"extra":true}`,
	} {
		var outcome ActorOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("json.Unmarshal(%s) error = nil", raw)
		}
	}
}

func TestWorkerActorOutcomeAcceptsZeroCursor(t *testing.T) {
	var outcome ActorOutcome
	if err := json.Unmarshal([]byte(`{"terminal_input_sequence":0,"succeeded":{}}`), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.TerminalInputSequence != 0 || outcome.Succeeded == nil || outcome.Failed != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
}
