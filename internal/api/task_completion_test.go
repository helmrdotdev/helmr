package api

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestWorkerCompleteTaskRequestRejectsAmbiguousWireShapes(t *testing.T) {
	validOutcome := `"outcome":{"succeeded":{"output":null}}`
	validWorkspace := `"workspace":{"captured":{"artifact":{}}}`
	invalid := [][]byte{
		[]byte(`{"lease":{},"lease":{},` + validOutcome + `,` + validWorkspace + `}`),
		[]byte(`{"lease":{"id":"first","id":"second"},` + validOutcome + `,` + validWorkspace + `}`),
		[]byte(`{"lease":{},"unknown":true,` + validOutcome + `,` + validWorkspace + `}`),
		append([]byte(`{"lease":{"id":"`), append([]byte{0xff}, []byte(`"},`+validOutcome+`,`+validWorkspace+`}`)...)...),
	}
	for _, raw := range invalid {
		var request WorkerCompleteTaskRequest
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
		var outcome WorkerTaskOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("ambiguous outcome %s was accepted", raw)
		}
	}
}

func TestWorkerTaskOutcomePreservesJSONNullOutput(t *testing.T) {
	var outcome WorkerTaskOutcome
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
		var outcome WorkerTaskOutcome
		if err := json.Unmarshal([]byte(raw), &outcome); err == nil {
			t.Fatalf("failure without a message %s was accepted", raw)
		}
	}

	var outcome WorkerTaskOutcome
	if err := json.Unmarshal([]byte(`{"failed":{"message":""}}`), &outcome); err != nil {
		t.Fatal(err)
	}
	if outcome.Failed == nil || outcome.Failed.Message != "" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestWorkerTaskWorkspaceProofRejectsAmbiguousWireShapes(t *testing.T) {
	invalid := []string{
		`{}`,
		`{"captured":null}`,
		`{"captured":{"artifact":{}},"rolled_back":{"base_workspace_version_id":"base"}}`,
		`{"rolled_back":{},"rolled_back":{"base_workspace_version_id":"base"}}`,
		`{"rolled_back":{"base_workspace_version_id":"base","unknown":true}}`,
	}
	for _, raw := range invalid {
		var proof WorkerTaskWorkspaceProof
		if err := json.Unmarshal([]byte(raw), &proof); err == nil {
			t.Fatalf("ambiguous Workspace proof %s was accepted", raw)
		}
	}
}

func TestWorkerWorkspaceResetTargetRequiresOneSource(t *testing.T) {
	valid := []string{
		`{"base_workspace_version_id":"base","tree":{},"empty":{}}`,
		`{"base_workspace_version_id":"base","tree":{},"artifact":{}}`,
	}
	for _, raw := range valid {
		var target WorkerWorkspaceResetTarget
		if err := json.Unmarshal([]byte(raw), &target); err != nil {
			t.Fatalf("valid target %s was rejected: %v", raw, err)
		}
	}
	invalid := []string{
		`{"base_workspace_version_id":"base","tree":{}}`,
		`{"base_workspace_version_id":"base","tree":{},"empty":{},"artifact":{}}`,
		`{"base_workspace_version_id":"base","tree":{},"empty":null}`,
		`{"base_workspace_version_id":"base","tree":{},"empty":{},"unknown":true}`,
	}
	for _, raw := range invalid {
		var target WorkerWorkspaceResetTarget
		if err := json.Unmarshal([]byte(raw), &target); err == nil {
			t.Fatalf("ambiguous target %s was accepted", raw)
		}
	}
}
