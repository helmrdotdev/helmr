package controlplane

import (
	"encoding/json"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestCheckpointCorrelationSelectsExactMember(t *testing.T) {
	cp := db.ComputerCheckpoint{ID: pgvalue.UUID(uuid.NewV7())}
	wait := db.RunWait{ID: pgvalue.UUID(uuid.NewV7()), RunID: pgvalue.UUID(uuid.NewV7()), AttemptNumber: 2}
	target := workerapi.CheckpointRun{RunID: pgvalue.UUIDString(wait.RunID), AttemptNumber: 2, RunWaitID: pgvalue.UUIDString(wait.ID), CorrelationID: "target"}
	for _, test := range []struct {
		name string
		runs []workerapi.CheckpointRun
		want bool
	}{
		{"target second", []workerapi.CheckpointRun{{RunID: uuid.NewV7().String(), CorrelationID: "peer"}, target}, true},
		{"missing", nil, false},
		{"duplicate", []workerapi.CheckpointRun{target, target}, false},
		{"wrong attempt", []workerapi.CheckpointRun{{RunID: target.RunID, AttemptNumber: 3, RunWaitID: target.RunWaitID, CorrelationID: target.CorrelationID}}, false},
		{"wrong wait", []workerapi.CheckpointRun{{RunID: target.RunID, AttemptNumber: 2, RunWaitID: uuid.NewV7().String(), CorrelationID: target.CorrelationID}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var err error
			cp.Manifest, err = json.Marshal(workerapi.CheckpointManifest{RecoveryPoint: workerapi.CheckpointRecoveryPoint{ID: pgvalue.UUIDString(cp.ID), Runs: test.runs}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := checkpointCorrelationID(cp, wait)
			if (err == nil) != test.want || (test.want && got != "target") {
				t.Fatalf("correlation=%q error=%v", got, err)
			}
		})
	}
}
