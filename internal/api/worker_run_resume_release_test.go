package api

import (
	"encoding/json"
	"testing"
)

func TestWorkerRunResumeReleaseRequestJSON(t *testing.T) {
	request := WorkerRunResumeReleaseRequest{
		Lease:                WorkerRunLeaseReceipt{ID: "lease-1"},
		RunWaitID:            "wait-1",
		CheckpointID:         "checkpoint-1",
		ResumeAttachID:       "attach-1",
		ResumeRequestVersion: 7,
		RunLeaseID:           "lease-1",
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"lease", "run_wait_id", "checkpoint_id", "resume_attach_id",
		"resume_request_version", "run_lease_id",
	} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("serialized request missing %q: %s", name, raw)
		}
	}
	if len(fields) != 6 {
		t.Fatalf("serialized request fields = %v", fields)
	}
	var decoded WorkerRunResumeReleaseRequest
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Lease.ID != request.Lease.ID ||
		decoded.RunWaitID != request.RunWaitID ||
		decoded.CheckpointID != request.CheckpointID ||
		decoded.ResumeAttachID != request.ResumeAttachID ||
		decoded.ResumeRequestVersion != request.ResumeRequestVersion ||
		decoded.RunLeaseID != request.RunLeaseID {
		t.Fatalf("decoded request = %+v", decoded)
	}
}

func TestWorkerRunResumeReleaseRequestRejectsOpenOrAmbiguousJSON(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"lease":null,"run_wait_id":"wait-1","checkpoint_id":"checkpoint-1","resume_attach_id":"attach-1","resume_request_version":7,"run_lease_id":"lease-1"}`),
		[]byte(`{"run_wait_id":"wait-1","checkpoint_id":"checkpoint-1","resume_attach_id":"attach-1","resume_request_version":7,"run_lease_id":"lease-1"}`),
		[]byte(`{"lease":{},"unknown":true}`),
		[]byte(`{"lease":{},"lease":{}}`),
		[]byte(`{"lease":{"id":"first","id":"second"}}`),
		[]byte(`{"lease":{}} {}`),
	} {
		var request WorkerRunResumeReleaseRequest
		if err := json.Unmarshal(raw, &request); err == nil {
			t.Fatalf("ambiguous request %q was accepted", raw)
		}
	}
}

func TestWorkerRunResumeReleaseResponseJSON(t *testing.T) {
	raw, err := json.Marshal(WorkerRunResumeReleaseResponse{
		Lease:                WorkerRunLeaseReceipt{ID: "lease-1"},
		RunWaitID:            "wait-1",
		CheckpointID:         "checkpoint-1",
		ResumeAttachID:       "attach-1",
		ResumeRequestVersion: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 5 {
		t.Fatalf("serialized response fields = %v", fields)
	}
	for _, name := range []string{"lease", "run_wait_id", "checkpoint_id", "resume_attach_id", "resume_request_version"} {
		if _, ok := fields[name]; !ok {
			t.Fatalf("serialized response missing %q: %s", name, raw)
		}
	}
}
