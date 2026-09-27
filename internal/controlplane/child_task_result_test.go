package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestChildTaskResultProjectsTerminalOutcome(t *testing.T) {
	runID := uuid.NewV7().String()

	tests := []struct {
		name string
		run  db.Run
		want string
	}{
		{
			name: "success",
			run: db.Run{
				ID:     pgvalue.UUID(uuid.MustParse(runID)),
				Status: db.RunStatusSucceeded,
				Output: json.RawMessage(`{"value":7}`),
			},
			want: `{"ok":true,"output":{"value":7},"run":{"id":"` + runID + `"}}`,
		},
		{
			name: "failure",
			run: db.Run{
				ID:     pgvalue.UUID(uuid.MustParse(runID)),
				Status: db.RunStatusFailed,
				Failure: json.RawMessage(
					`{"code":"upstream_failed","message":"upstream failed","details":{"service":"images"}}`,
				),
			},
			want: `{"ok":false,"failure":{"code":"upstream_failed","message":"upstream failed","details":{"service":"images"}},"run":{"id":"` + runID + `"}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := childTaskResult(test.run)
			if err != nil {
				t.Fatalf("childTaskResult() error = %v", err)
			}
			if string(got) != test.want {
				t.Fatalf("childTaskResult() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestChildTaskResultRejectsIncompleteTerminalState(t *testing.T) {
	_, err := childTaskResult(db.Run{
		ID:     pgvalue.UUID(uuid.NewV7()),
		Status: db.RunStatusFailed,
	})
	if err == nil {
		t.Fatal("childTaskResult() accepted a failed Run without a terminal reason")
	}
}
