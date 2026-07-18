package dispatch

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestBuildQueueMessageFreezesCandidateFenceAndHostResourceVector(t *testing.T) {
	now := time.Now().UTC()
	row := db.ListQueuedDeploymentBuildCandidatesRow{
		OrgID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ProjectID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DeploymentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		BuildRegionID: "us-east-1", LeaseSequence: 3, QueueTimestamp: pgvalue.Timestamptz(now),
		BuildRequestedCpuMillis: 2000, BuildRequestedMemoryBytes: 2 << 30,
		BuildRequestedWorkloadDiskBytes: 0, BuildRequestedScratchBytes: 13 << 30, BuildRequestedExecutors: 1,
	}
	message, err := buildQueueMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	if message.WorkKind != WorkKindBuild || message.ReadyFence() != "build:3" || !message.QueueTimestamp.Equal(now) {
		t.Fatalf("build message fence = %+v", message)
	}
	want := (BuildResourceVector{CPUMillis: 2000, MemoryBytes: 2 << 30,
		WorkloadDiskBytes: 0, ScratchBytes: 13 << 30, Executors: 1})
	if message.BuildResources != want {
		t.Fatalf("build resources = %+v, want %+v", message.BuildResources, want)
	}
}
