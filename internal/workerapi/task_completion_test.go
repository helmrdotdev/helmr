package workerapi

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWorkerCompleteTaskRequestRejectsAmbiguousWireShapes(t *testing.T) {
	validOutcome := `"outcome":{"succeeded":{"output":null}}`
	validOperation := `"operation_id":"operation"`
	invalid := [][]byte{
		[]byte(`{"lease":{},"lease":{},` + validOutcome + `,` + validOperation + `}`),
		[]byte(`{"lease":{"id":"first","id":"second"},` + validOutcome + `,` + validOperation + `}`),
		[]byte(`{"lease":{},"unknown":true,` + validOutcome + `,` + validOperation + `}`),
		append([]byte(`{"lease":{"id":"`), append([]byte{0xff}, []byte(`"},`+validOutcome+`,`+validOperation+`}`)...)...),
	}
	for _, raw := range invalid {
		var request CompleteTaskRequest
		if err := json.NewDecoder(bytes.NewReader(raw)).Decode(&request); err == nil {
			t.Fatalf("ambiguous request %q was accepted", raw)
		}
	}
}

func TestWorkerTaskOutcomeRejectsAmbiguousWireShapes(t *testing.T) {
	invalid := []string{
		`{}`,
		`{"succeeded":null}`,
		`{"succeeded":{"output":null},"failed":{"message":"failed"}}`,
		`{"succeeded":{"output":null},"succeeded":{"output":1}}`,
		`{"succeeded":{"output":null,"unknown":true}}`,
		`{"unknown":{"output":null}}`,
	}
	for _, raw := range invalid {
		var outcome TaskOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("ambiguous outcome %s was accepted", raw)
		}
	}
}

func TestWorkerTaskOutcomePreservesJSONNullOutput(t *testing.T) {
	var outcome TaskOutcome
	if err := json.Unmarshal([]byte(`{"succeeded":{"output":null}}`), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Succeeded == nil || string(outcome.Succeeded.Output) != "null" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestWorkerTaskFailureRequiresMessagePresence(t *testing.T) {
	for _, raw := range []string{
		`{"failed":{}}`,
		`{"failed":{"message":null}}`,
		`{"payload_invalid":{"details":null}}`,
	} {
		var outcome TaskOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("failure without a message %s was accepted", raw)
		}
	}

	var outcome TaskOutcome
	if err := json.Unmarshal([]byte(`{"failed":{"message":""}}`), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Failed == nil || outcome.Failed.Message != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestWorkerComputerMountTargetRequiresVersionOnly(t *testing.T) {
	valid := []string{
		`{"base_computer_disk_version_id":"base"}`,
	}
	for _, raw := range valid {
		var target ComputerMountTarget
		if err := json.Unmarshal([]byte(raw), &target); err != nil {
			t.Fatalf("valid target %s was rejected: %v", raw, err)
		}
	}
	invalid := []string{
		`{"base_computer_disk_version_id":"base","tree":{}}`,
		`{"base_computer_disk_version_id":"base","tree":{},"empty":{},"artifact":{}}`,
		`{"base_computer_disk_version_id":"base","tree":{},"empty":null}`,
		`{"base_computer_disk_version_id":"base","tree":{},"empty":{},"unknown":true}`,
	}
	for _, raw := range invalid {
		var target ComputerMountTarget
		if err := json.Unmarshal([]byte(raw), &target); err == nil {
			t.Fatalf("ambiguous target %s was accepted", raw)
		}
	}
}
