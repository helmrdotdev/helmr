package workerapi

import (
	"encoding/json"
	"testing"
)

func TestComputerRestoreAcknowledgementClosedJSON(t *testing.T) {
	valid := `{"computer_instance_id":"instance","checkpoint_id":"checkpoint","desired_version":1,"writer_generation":2,"grants":[{"run_id":"run","lease":{"id":"lease","lease_sequence":2}}]}`
	var request ComputerRestoreAckRequest
	if err := json.Unmarshal([]byte(valid), &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Grants) != 1 || request.Grants[0].Lease.LeaseSequence != 2 {
		t.Fatal("grant lost in decode")
	}
	for _, raw := range []string{
		`{"grants":[],"grants":[]}`,
		`{"grants":[],"unexpected":1}`,
		`{"grants":[{"run_id":"r","run_id":"other","lease":{}}]}`,
		`{"grants":[{"run_id":"r","lease":{"id":"a","id":"b"}}]}`,
		`{"grants":[{"run_id":"r","lease":{"id":"a","unexpected":1}}]}`,
		`{"grants":[{"run_id":"r","lease":{},"unexpected":1}]}`,
	} {
		if err := json.Unmarshal([]byte(raw), &request); err == nil {
			t.Fatalf("accepted ambiguous authority: %s", raw)
		}
	}
}
