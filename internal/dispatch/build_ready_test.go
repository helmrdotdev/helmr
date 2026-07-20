package dispatch

import (
	"encoding/json"
	"strings"
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
		BuildArchitecture:       "aarch64",
		BuildRequestedCpuMillis: 3000, BuildRequestedMemoryBytes: 4 << 30,
		BuildRequestedWorkloadDiskBytes: 0, BuildRequestedScratchBytes: 32 << 30, BuildRequestedExecutors: 1,
	}
	message, err := buildQueueMessage(row)
	if err != nil {
		t.Fatal(err)
	}
	if message.WorkKind != WorkKindBuild || message.ReadyFence() != "build:3" ||
		!message.QueueOriginAt.Equal(now) || !message.QueueScoreAt.Equal(now) {
		t.Fatalf("build message fence = %+v", message)
	}
	if message.BuildArchitecture != "aarch64" {
		t.Fatalf("build architecture = %q, want aarch64", message.BuildArchitecture)
	}
	want := (BuildResourceVector{CPUMillis: 3000, MemoryBytes: 4 << 30,
		WorkloadDiskBytes: 0, ScratchBytes: 32 << 30, Executors: 1})
	if message.BuildResources != want {
		t.Fatalf("build resources = %+v, want %+v", message.BuildResources, want)
	}
}

func TestBuildReadyMessageRejectsMalformedArchitecture(t *testing.T) {
	message := Message{
		WorkKind: WorkKindBuild, DeploymentID: uuid.NewString(), OrgID: uuid.NewString(),
		ProjectID: uuid.NewString(), EnvironmentID: uuid.NewString(), RegionID: "us-east-1",
		QueueName: "deployment-build", LeaseSequence: 1,
		BuildArchitecture: "amd64",
		BuildResources: BuildResourceVector{CPUMillis: 3000, MemoryBytes: 4 << 30,
			ScratchBytes: 32 << 30, Executors: 1},
	}
	if err := message.Validate(); err == nil {
		t.Fatal("Validate() accepted external architecture spelling amd64")
	}
}

func TestBuildReadyMessageArchitectureRoundTripDoesNotAdvertiseRuntimeDigest(t *testing.T) {
	queuedAt := time.Now().UTC()
	message := Message{
		WorkKind: WorkKindBuild, DeploymentID: uuid.NewString(), OrgID: uuid.NewString(),
		ProjectID: uuid.NewString(), EnvironmentID: uuid.NewString(), RegionID: "us-east-1",
		QueueName: "deployment-build", LeaseSequence: 2,
		BuildArchitecture: "x86_64",
		QueueOriginAt:     queuedAt,
		QueueScoreAt:      queuedAt,
		BuildResources: BuildResourceVector{CPUMillis: 3000, MemoryBytes: 4 << 30,
			ScratchBytes: 32 << 30, Executors: 1},
	}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "BuildRuntimeDigest") ||
		strings.Contains(string(raw), "RuntimeDigest") ||
		strings.Contains(string(raw), "sha256:") {
		t.Fatalf("ready message advertises runtime digest: %s", raw)
	}
	var roundTrip Message
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.BuildArchitecture != message.BuildArchitecture {
		t.Fatalf("round-trip architecture = %q, want %q", roundTrip.BuildArchitecture, message.BuildArchitecture)
	}
	if err := roundTrip.Validate(); err != nil {
		t.Fatalf("round-trip message invalid: %v", err)
	}
}
