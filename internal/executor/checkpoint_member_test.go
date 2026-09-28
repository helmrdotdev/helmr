package executor

import (
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"testing"
)

func TestRestoredCheckpointMemberSelection(t *testing.T) {
	target := workerapi.CheckpointRun{RunID: "target", AttemptNumber: 2, RunWaitID: "wait", CorrelationID: "correlation"}
	for _, test := range []struct {
		name string
		runs []workerapi.CheckpointRun
		want bool
	}{
		{"target second", []workerapi.CheckpointRun{{RunID: "peer"}, target}, true},
		{"missing", []workerapi.CheckpointRun{{RunID: "peer"}}, false},
		{"duplicate", []workerapi.CheckpointRun{target, target}, false},
		{"wrong wait", []workerapi.CheckpointRun{{RunID: "target", AttemptNumber: 2, RunWaitID: "other", CorrelationID: "correlation"}}, false},
		{"wrong attempt", []workerapi.CheckpointRun{{RunID: "target", AttemptNumber: 3, RunWaitID: "wait", CorrelationID: "correlation"}}, false},
		{"wrong correlation", []workerapi.CheckpointRun{{RunID: "target", AttemptNumber: 2, RunWaitID: "wait", CorrelationID: "other"}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateRestoredCheckpointMember(workerapi.CheckpointRecoveryPoint{Runs: test.runs}, "target", 2, "wait", "correlation")
			if (err == nil) != test.want {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
