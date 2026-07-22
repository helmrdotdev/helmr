package api

import (
	"encoding/json"
	"testing"
)

func TestWorkerRunStartRequestClosedUnion(t *testing.T) {
	validUUIDs := `"run_wait_id":"00000000-0000-0000-0000-000000000001",` +
		`"checkpoint_id":"00000000-0000-0000-0000-000000000002",` +
		`"resume_attach_id":"00000000-0000-0000-0000-000000000003"`
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "fresh", body: `{"lease":{},"fresh":{}}`, ok: true},
		{name: "restore", body: `{"lease":{},"restore":{` + validUUIDs + `,"resume_request_version":1}}`, ok: true},
		{name: "child", body: `{"lease":{},"attach":{"child":{` + validUUIDs + `}}}`, ok: true},
		{name: "parent", body: `{"lease":{},"attach":{"parent":{` + validUUIDs + `,"resume_request_version":1}}}`, ok: true},
		{name: "missing arm", body: `{"lease":{}}`},
		{name: "multiple arms", body: `{"lease":{},"fresh":{},"restore":{}}`},
		{name: "null arm", body: `{"lease":{},"fresh":null}`},
		{name: "missing attach arm", body: `{"lease":{},"attach":{}}`},
		{name: "multiple attach arms", body: `{"lease":{},"attach":{"child":{},"parent":{}}}`},
		{name: "null attach arm", body: `{"lease":{},"attach":{"child":null}}`},
		{name: "unknown top level", body: `{"lease":{},"fresh":{},"unknown":true}`},
		{name: "unknown nested", body: `{"lease":{},"fresh":{"unknown":true}}`},
		{name: "unknown attach nested", body: `{"lease":{},"attach":{"child":{"unknown":true}}}`},
		{name: "null lease", body: `{"lease":null,"fresh":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request WorkerRunStartRequest
			err := json.Unmarshal([]byte(test.body), &request)
			if (err == nil) != test.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWorkerRunStartRequestMarshalUsesSelectedArm(t *testing.T) {
	data, err := json.Marshal(WorkerRunStartRequest{
		Lease: WorkerRunLeaseReceipt{ID: "lease"},
		Fresh: &WorkerRunStartFresh{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 2 || fields["fresh"] == nil || fields["lease"] == nil {
		t.Fatalf("body = %s", data)
	}
}
