package workerapi

import (
	"encoding/json"
	"testing"
)

func TestWorkerRunStartRequestRequiresClosedLease(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"lease", `{"lease":{"id":"lease","lease_sequence":2}}`, true},
		{"missing lease", `{}`, false},
		{"null lease", `{"lease":null}`, false},
		{"unknown field", `{"lease":{},"unknown":true}`, false},
		{"unknown lease field", `{"lease":{"unknown":true}}`, false},
		{"invalid sequence type", `{"lease":{"lease_sequence":"two"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var request RunStartRequest
			err := json.Unmarshal([]byte(tc.body), &request)
			if (err == nil) != tc.valid {
				t.Fatalf("decode: %v", err)
			}
			if tc.valid && request.Lease != (RunLeaseFence{ID: "lease", LeaseSequence: 2}) {
				t.Fatalf("lease: %+v", request.Lease)
			}
		})
	}
}
