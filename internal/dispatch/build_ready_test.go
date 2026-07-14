package dispatch

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestBuildQueueMessageFreezesCandidateFenceAndFullResourceVector(t *testing.T) {
	now := time.Now().UTC()
	row := db.ListQueuedDeploymentBuildCandidatesRow{
		OrgID: pgvalue.UUID(uuid.Must(uuid.NewV7())), ProjectID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		EnvironmentID: pgvalue.UUID(uuid.Must(uuid.NewV7())), DeploymentID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		BuildRegionID: "us-east-1", BuildAttemptNumber: 3, LeaseSequence: 7, QueueTimestamp: pgvalue.Timestamptz(now),
		BuildRequestedCpuMillis: 1000, BuildRequestedMemoryBytes: 2048, BuildRequestedWorkloadDiskBytes: 4096,
		BuildRequestedScratchBytes: 8192, BuildRequestedBuildCacheBytes: 16384,
		BuildRequestedArtifactCacheBytes: 32768, BuildRequestedExecutors: 2,
	}
	message, err := buildQueueMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	if message.WorkKind != WorkKindBuild || message.ReadyFence() != "build:3:7" || !message.QueueTimestamp.Equal(now) {
		t.Fatalf("build message fence = %+v", message)
	}
	want := (BuildResourceVector{CPUMillis: 1000, MemoryBytes: 2048, WorkloadDiskBytes: 4096,
		ScratchBytes: 8192, BuildCacheBytes: 16384, ArtifactCacheBytes: 32768, Executors: 2})
	if message.BuildResources != want {
		t.Fatalf("build resources = %+v, want %+v", message.BuildResources, want)
	}
}
