package control

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestParseCheckpointReadyRequestBindsFullAtomicProof(t *testing.T) {
	request := validCheckpointReadyRequest()
	parsed, normalized, err := parseCheckpointReadyRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.waitID.String() != request.RunWaitID || parsed.checkpointID.String() != request.CheckpointID ||
		parsed.requestVersion != request.RequestVersion || parsed.attemptNumber != request.Lease.AttemptNumber ||
		parsed.capture.tree.Digest != request.WorkspaceCapture.Tree.Digest || len(parsed.artifacts) != 4 ||
		parsed.fingerprint == "" || len(parsed.manifest) == 0 {
		t.Fatalf("parsed checkpoint-ready = %+v", parsed)
	}
	if !normalized.Lease.ExpiresAt.Equal(request.Lease.ExpiresAt) {
		t.Fatalf("normalized expiry = %s", normalized.Lease.ExpiresAt)
	}

	changed := request
	changed.WorkspaceCapture.Tree.Digest = digestWith("9")
	changedParsed, _, err := parseCheckpointReadyRequest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedParsed.fingerprint == parsed.fingerprint {
		t.Fatal("changed Workspace proof retained checkpoint-ready fingerprint")
	}
}

func TestParseCheckpointReadyRequestRejectsMismatchedRecoveryPoint(t *testing.T) {
	request := validCheckpointReadyRequest()
	request.Manifest.RecoveryPoint.RunWaitID = uuid.Must(uuid.NewV7()).String()
	if _, _, err := parseCheckpointReadyRequest(request); err == nil || !strings.Contains(err.Error(), "recovery_point") {
		t.Fatalf("err = %v, want recovery point mismatch", err)
	}
}

func TestParseCheckpointFailedRequestBindsNormalizedFailure(t *testing.T) {
	ready := validCheckpointReadyRequest()
	request := api.WorkerCheckpointFailedRequest{
		Lease: ready.Lease, RequestVersion: ready.RequestVersion,
		RunWaitID: ready.RunWaitID, CheckpointID: ready.CheckpointID, Error: "  snapshot failed  ",
	}
	parsed, normalized, err := parseCheckpointFailedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.waitID.String() != request.RunWaitID || parsed.checkpointID.String() != request.CheckpointID ||
		parsed.requestVersion != request.RequestVersion || parsed.attemptNumber != request.Lease.AttemptNumber ||
		parsed.fingerprint == "" || normalized.Error != "snapshot failed" ||
		!strings.Contains(string(parsed.errorPayload), `"message":"snapshot failed"`) {
		t.Fatalf("parsed checkpoint failure = %+v, normalized=%+v", parsed, normalized)
	}
	changed := request
	changed.Error = "different failure"
	changedParsed, _, err := parseCheckpointFailedRequest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if changedParsed.fingerprint == parsed.fingerprint {
		t.Fatal("different checkpoint failure retained request fingerprint")
	}
}

func TestTokenWaitDecisionMapsExpiredTokenFailureToTimeout(t *testing.T) {
	kind, payload, err := tokenWaitDecision("failed", nil, "token_expired")
	if err != nil || kind != "timed_out" || string(payload) != "null" {
		t.Fatalf("decision = %q %s, err=%v", kind, payload, err)
	}
}

func validCheckpointReadyRequest() api.WorkerCheckpointReadyRequest {
	runID := uuid.Must(uuid.NewV7()).String()
	waitID := uuid.Must(uuid.NewV7()).String()
	checkpointID := uuid.Must(uuid.NewV7()).String()
	runtimeIdentity := digestWith("1")
	lease := api.WorkerRunLeaseReceipt{
		ID: uuid.Must(uuid.NewV7()).String(), RunID: runID, AttemptNumber: 1, LeaseSequence: 1,
		WorkerGroupID: "run-test", WorkerInstanceID: uuid.Must(uuid.NewV7()).String(), WorkerEpoch: 1,
		WorkerProtocolVersion: api.CurrentWorkerProtocolVersion,
		RuntimeInstanceID:     uuid.Must(uuid.NewV7()).String(), RuntimeIdentityID: runtimeIdentity,
		NetworkSlotID: uuid.Must(uuid.NewV7()).String(), NetworkSlotGeneration: 1,
		WorkspaceID: uuid.Must(uuid.NewV7()).String(), WorkspaceMountID: uuid.Must(uuid.NewV7()).String(),
		WorkspaceLeaseID: uuid.Must(uuid.NewV7()).String(), BaseWorkspaceVersionID: uuid.Must(uuid.NewV7()).String(),
		OwnershipGeneration: 1, WriterGeneration: 1, MountFencingGeneration: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedExecutionSlots: 1,
		MaxActiveDurationMs: 60_000, StartDeadlineAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	return api.WorkerCheckpointReadyRequest{
		Lease: lease, RequestVersion: 1, RunWaitID: waitID, CheckpointID: checkpointID,
		WorkspaceCapture: api.WorkerCheckpointWorkspaceCapture{
			Tree: api.WorkerWorkspaceTreeIdentity{Digest: digestWith("2"), SizeBytes: 10, EntryCount: 1},
			Artifact: api.WorkerWorkspaceArtifact{
				Digest: digestWith("3"), MediaType: workspace.ArtifactMediaType,
				Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 1,
			},
		},
		Manifest: api.WorkerCheckpointManifest{
			RecoveryPoint: api.WorkerCheckpointRecoveryPoint{
				ID: checkpointID, RunID: runID, AttemptNumber: 1, RunWaitID: waitID,
				CorrelationID: uuid.Must(uuid.NewV7()).String(),
				Runtime: api.WorkerCheckpointRuntime{
					Backend: "firecracker", ID: runtimeIdentity, Arch: string(deployment.ArchitectureAArch64),
					ABI: "helmr.firecracker.snapshot.v0", KernelDigest: digestWith("4"),
					InitramfsDigest: digestWith("5"), RootfsDigest: digestWith("6"), ConfigDigest: digestWith("7"),
				},
			},
			RuntimeState: api.WorkerCheckpointRuntimeState{
				ConfigArtifact:      api.WorkerCheckpointArtifact{Digest: digestWith("a"), SizeBytes: 100, MediaType: cas.CheckpointRuntimeConfigMediaType},
				VMStateArtifact:     api.WorkerCheckpointArtifact{Digest: digestWith("b"), SizeBytes: 100, MediaType: cas.CheckpointVMStateMediaType},
				ScratchDiskArtifact: api.WorkerCheckpointArtifact{Digest: digestWith("c"), SizeBytes: 100, MediaType: cas.CheckpointScratchDiskMediaType},
				MemoryArtifacts:     []api.WorkerCheckpointArtifact{{Digest: digestWith("d"), SizeBytes: 100, MediaType: cas.CheckpointMemoryMediaType}},
				Config:              json.RawMessage(`{"machine-config":{"vcpu_count":1}}`),
			},
		},
	}
}

func digestWith(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
