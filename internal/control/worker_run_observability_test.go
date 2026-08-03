package control

import (
	"encoding/json"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestRunMetadataMutationNormalizesAndAppliesInApplication(t *testing.T) {
	amount := 2.0
	tests := []struct {
		name    string
		current json.RawMessage
		request api.WorkerUpdateRunMetadataRequest
		want    string
	}{
		{
			name:    "set",
			current: json.RawMessage(`{"phase":"queued"}`),
			request: api.WorkerUpdateRunMetadataRequest{
				Operation: "set", Key: "phase",
				Value: json.RawMessage(`"running"`),
			},
			want: `{"phase":"running"}`,
		},
		{
			name:    "patch",
			current: json.RawMessage(`{"phase":"running","steps":1}`),
			request: api.WorkerUpdateRunMetadataRequest{
				Operation: "patch",
				Patch:     json.RawMessage(`{"approved":true,"phase":"done"}`),
			},
			want: `{"approved":true,"phase":"done","steps":1}`,
		},
		{
			name:    "increment",
			current: json.RawMessage(`{"steps":1}`),
			request: api.WorkerUpdateRunMetadataRequest{
				Operation: "increment", Key: "steps", Amount: &amount,
			},
			want: `{"steps":3}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mutation, err := normalizeRunMetadataMutation(test.request)
			if err != nil {
				t.Fatal(err)
			}
			got, err := applyRunMetadataMutation(test.current, mutation)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := normalizeMetadata(got, maxRunMetadataBytes, "Run")
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != test.want {
				t.Fatalf("metadata = %s, want %s", canonical, test.want)
			}
		})
	}
}

func TestRunMetadataIncrementRejectsNonnumericStoredValue(t *testing.T) {
	amount := 1.0
	mutation, err := normalizeRunMetadataMutation(
		api.WorkerUpdateRunMetadataRequest{
			Operation: "increment", Key: "steps", Amount: &amount,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := applyRunMetadataMutation(
		json.RawMessage(`{"steps":"one"}`),
		mutation,
	); err == nil {
		t.Fatal("expected nonnumeric increment rejection")
	}
}
