package lifetime

import (
	"encoding/json"
	"testing"
)

func TestRetentionPolicyPreservesOmissionNeverAndFinite(t *testing.T) {
	type request struct {
		Policy Policy `json:"policy,omitzero"`
	}
	for _, raw := range []string{`{}`, `{"policy":null}`, `{"policy":604800000}`} {
		var value request
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != raw {
			t.Fatalf("tri-state changed %s: %s %v", raw, encoded, err)
		}
	}
	for _, raw := range []string{`0`, `-1`, `1.5`, `9007199254740992`, `"never"`, `true`, `{}`} {
		var value Policy
		if err := json.Unmarshal([]byte(raw), &value); err == nil {
			t.Fatalf("invalid policy accepted: %s", raw)
		}
	}
}
