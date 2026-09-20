package workerapi

import (
	"encoding/json"
	"testing"
)

func TestWorkerActorOutcomeRequiresPositiveGenerationAndOneVariant(t *testing.T) {
	for _, raw := range []string{
		`{"succeeded":{}}`,
		`{"run_generation":1}`,
		`{"run_generation":1,"succeeded":{},"failed":{"message":"x"}}`,
		`{"run_generation":-1,"succeeded":{}}`,
		`{"run_generation":0,"succeeded":{}}`,
		`{"run_generation":1,"interrupted":null}`,
		`{"run_generation":1,"succeeded":null}`,
		`{"run_generation":1,"succeeded":{},"extra":true}`,
	} {
		var outcome ActorOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("json.Unmarshal(%s) error = nil", raw)
		}
	}
}

func TestWorkerActorOutcomeAcceptsGeneration(t *testing.T) {
	var outcome ActorOutcome
	if err := json.Unmarshal([]byte(`{"run_generation":1,"succeeded":{}}`), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.RunGeneration != 1 || outcome.Succeeded == nil || outcome.Failed != nil {
		t.Fatalf("outcome = %#v", outcome)
	}
}

func TestWorkerActorInterruptedOutcomeRoundTrip(t *testing.T) {
	for _, turn := range []string{`null`, `"01900000-0000-7000-8000-000000000003"`} {
		raw := `{"run_generation":3,"interrupted":{"hold_id":"01900000-0000-7000-8000-000000000002","turn_id":` + turn + `}}`
		var outcome ActorOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err != nil {
			t.Fatal(err)
		}
		if outcome.RunGeneration != 3 || outcome.Interrupted == nil || outcome.Succeeded != nil || outcome.Failed != nil {
			t.Fatalf("outcome: %+v", outcome)
		}
		encoded, err := json.Marshal(outcome)
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != raw {
			t.Fatalf("round trip: %s", encoded)
		}
	}
}
