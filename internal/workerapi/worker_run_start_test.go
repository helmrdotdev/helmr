package workerapi

import (
	"encoding/json"
	"testing"
)

func TestWorkerRunStartRequestClosedUnion(t *testing.T) {
	tests := []struct {
		name string
		body string
		ok   bool
	}{
		{name: "fresh", body: `{"lease":{},"fresh":{}}`, ok: true},
		{name: "restore", body: `{"lease":{},"restore":{}}`, ok: true},
		{name: "missing arm", body: `{"lease":{}}`},
		{name: "multiple arms", body: `{"lease":{},"fresh":{},"restore":{}}`},
		{name: "null arm", body: `{"lease":{},"fresh":null}`},
		{name: "removed attach arm", body: `{"lease":{},"attach":{"child":{}}}`},
		{name: "unknown top level", body: `{"lease":{},"fresh":{},"unknown":true}`},
		{name: "unknown nested", body: `{"lease":{},"fresh":{"unknown":true}}`},
		{name: "null lease", body: `{"lease":null,"fresh":{}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var request RunStartRequest
			err := json.Unmarshal([]byte(test.body), &request)
			if (err == nil) != test.ok {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestWorkerRunStartRequestMarshalUsesSelectedArm(t *testing.T) {
	data, err := json.Marshal(RunStartRequest{
		Lease: RunLeaseFence{ID: "lease", LeaseSequence: 1},
		Fresh: &RunStartFresh{},
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
