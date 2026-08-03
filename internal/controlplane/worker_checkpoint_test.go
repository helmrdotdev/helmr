package controlplane

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func TestParseCheckpointReadyRequestBindsFullAtomicProof(t *testing.T) {
	request := validCheckpointReadyRequest()
	parsed, normalized, err := parseCheckpointReadyRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.waitID.String() != request.RunWaitID || parsed.checkpointID.String() != request.CheckpointID ||
		parsed.requestVersion != request.RequestVersion ||
		parsed.capture.tree.Digest != request.WorkspaceCapture.Tree.Digest || len(parsed.artifacts) != 4 ||
		parsed.fingerprint == "" || len(parsed.manifest) == 0 {
		t.Fatalf("parsed checkpoint-ready = %+v", parsed)
	}
	if normalized.Lease != request.Lease {
		t.Fatalf("normalized fence = %+v", normalized.Lease)
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
	request := workerapi.CheckpointFailedRequest{
		Lease: ready.Lease, RequestVersion: ready.RequestVersion,
		RunWaitID: ready.RunWaitID, CheckpointID: ready.CheckpointID, Error: "  snapshot failed  ",
	}
	parsed, normalized, err := parseCheckpointFailedRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.waitID.String() != request.RunWaitID || parsed.checkpointID.String() != request.CheckpointID ||
		parsed.requestVersion != request.RequestVersion ||
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

func TestTokenWaitDecisionPreservesExpiredTokenFailure(t *testing.T) {
	kind, payload, err := tokenWaitDecision("failed", nil, "token_expired")
	if err != nil || kind != "failed" || string(payload) != `{"reason_code":"token_expired"}` {
		t.Fatalf("decision = %q %s, err=%v", kind, payload, err)
	}
}

func TestTokenWaitDecisionIncludesCancellationReason(t *testing.T) {
	kind, payload, err := tokenWaitDecision("cancelled", nil, "token_cancelled")
	if err != nil || kind != "cancelled" || string(payload) != `{"reason_code":"token_cancelled"}` {
		t.Fatalf("decision = %q %s, err=%v", kind, payload, err)
	}
}

func TestDecideActorCheckpointFailureStopsAtRunExpiry(t *testing.T) {
	failedAt := time.Date(2026, time.July, 23, 12, 0, 0, 0, time.UTC)
	authority := runLeaseClaimAuthority{
		run: db.Run{
			MaxActiveDurationMs: 300_000,
			RetryPolicy:         []byte(`{"enabled":true,"maxAttempts":3,"backoff":{"minMs":1,"maxMs":1,"factor":1,"jitter":"none"}}`),
		},
		attempt: db.RunAttempt{Number: 1},
		actor:   db.Actor{State: "open"},
	}
	decision, err := decideActorCheckpointFailure(authority, failedAt, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !decision.retry || decision.reason != "checkpoint_failed" {
		t.Fatalf("retry decision = %+v", decision)
	}

	decision, err = decideActorCheckpointFailure(authority, failedAt, authority.run.MaxActiveDurationMs)
	if err != nil {
		t.Fatal(err)
	}
	if decision.retry || decision.reason != "max_active_duration_exceeded" {
		t.Fatalf("Run duration decision = %+v", decision)
	}
}

func validCheckpointReadyRequest() workerapi.CheckpointReadyRequest {
	runID := uuid.Must(uuid.NewV7()).String()
	waitID := uuid.Must(uuid.NewV7()).String()
	checkpointID := uuid.Must(uuid.NewV7()).String()
	runtimeIdentity := digestWith("1")
	lease := workerapi.RunLeaseAssignment{
		ID: uuid.Must(uuid.NewV7()).String(), RunID: runID, AttemptNumber: 1, LeaseSequence: 1,
		WorkerGroupID: "run-test", WorkerInstanceID: uuid.Must(uuid.NewV7()).String(), WorkerEpoch: 1,
		WorkerProtocolVersion: workerapi.CurrentProtocolVersion,
		RuntimeInstanceID:     uuid.Must(uuid.NewV7()).String(), RuntimeIdentityID: runtimeIdentity,
		WorkspaceID: uuid.Must(uuid.NewV7()).String(), WorkspaceMountID: uuid.Must(uuid.NewV7()).String(),
		WorkspaceLeaseID: uuid.Must(uuid.NewV7()).String(), BaseWorkspaceVersionID: uuid.Must(uuid.NewV7()).String(),
		OwnershipGeneration: 1, WriterGeneration: 1, MountFencingGeneration: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedExecutionSlots: 1,
		MaxActiveDurationMs: 60_000, StartDeadlineAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	return workerapi.CheckpointReadyRequest{
		Lease: lease.Fence(), RequestVersion: 1, RunWaitID: waitID, CheckpointID: checkpointID,
		WorkspaceCapture: workerapi.CheckpointWorkspaceCapture{
			Tree: workerapi.WorkspaceTreeIdentity{Digest: digestWith("2"), SizeBytes: 10, EntryCount: 1},
			Artifact: workerapi.WorkspaceArtifact{
				Digest: digestWith("3"), MediaType: workspace.ArtifactMediaType,
				Encoding: workspace.ArtifactEncoding, SizeBytes: 1024, EntryCount: 1,
			},
		},
		Manifest: workerapi.CheckpointManifest{
			RecoveryPoint: workerapi.CheckpointRecoveryPoint{
				ID: checkpointID, RunID: runID, AttemptNumber: 1, RunWaitID: waitID,
				CorrelationID: uuid.Must(uuid.NewV7()).String(),
				Runtime: workerapi.CheckpointRuntime{
					Backend: "firecracker", ID: runtimeIdentity, Arch: string(deployment.ArchitectureX8664),
					ABI: "helmr.firecracker.snapshot.v0", KernelDigest: digestWith("4"),
					InitramfsDigest: digestWith("5"), RootfsDigest: digestWith("6"), ConfigDigest: digestWith("7"),
				},
			},
			RuntimeState: workerapi.CheckpointRuntimeState{
				ConfigArtifact:      workerapi.CheckpointArtifact{Digest: digestWith("a"), SizeBytes: 100, MediaType: cas.CheckpointRuntimeConfigMediaType},
				VMStateArtifact:     workerapi.CheckpointArtifact{Digest: digestWith("b"), SizeBytes: 100, MediaType: cas.CheckpointVMStateMediaType},
				ScratchDiskArtifact: workerapi.CheckpointArtifact{Digest: digestWith("c"), SizeBytes: 100, MediaType: cas.CheckpointScratchDiskMediaType},
				MemoryArtifacts:     []workerapi.CheckpointArtifact{{Digest: digestWith("d"), SizeBytes: 100, MediaType: cas.CheckpointMemoryMediaType}},
				Config:              json.RawMessage(`{"machine-config":{"vcpu_count":1}}`),
			},
		},
	}
}

func digestWith(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
