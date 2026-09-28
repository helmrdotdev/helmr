package workerapi

import (
	"encoding/json"
	"testing"
)

func TestRunWaitResumeAcknowledgementJSON(t *testing.T) {
	request := RunWaitResumeAckRequest{Lease: RunLeaseFence{ID: "lease", LeaseSequence: 2}, RunWaitID: "wait", CheckpointID: "checkpoint"}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var decoded RunWaitResumeAckRequest
	if err = json.Unmarshal(raw, &decoded); err != nil || decoded != request {
		t.Fatalf("roundtrip=%+v %v", decoded, err)
	}
	for _, raw := range []string{
		`{}`, `{"lease":null}`, `{"lease":{"id":"lease","lease_sequence":2,"unexpected":true}}`,
		`{"lease":{"id":"lease","lease_sequence":2},"unexpected":true}`,
		`{"lease":{"id":"lease","lease_sequence":2},"run_wait_id":"one","run_wait_id":"two"}`,
	} {
		if err = json.Unmarshal([]byte(raw), &decoded); err == nil {
			t.Fatalf("ambiguous acknowledgement accepted: %s", raw)
		}
	}
}
