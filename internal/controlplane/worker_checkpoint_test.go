package controlplane

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
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
		parsed.fingerprint == "" || len(parsed.manifest) == 0 || len(parsed.cleanupProof) == 0 {
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
	changed = request
	cleanup := *request.SourceCleanup
	changed.SourceCleanup = &cleanup
	changed.SourceCleanup.CompletedAt = changed.SourceCleanup.CompletedAt.Add(-time.Second)
	changedParsed, _, err = parseCheckpointReadyRequest(changed)
	if err != nil {
		t.Fatal(err)
	}
	retained := request
	retained.SourceCleanup = nil
	retainedParsed, _, err := parseCheckpointReadyRequest(retained)
	if err != nil {
		t.Fatal(err)
	}
	if retainedParsed.cleanupProof != nil || retainedParsed.fingerprint == parsed.fingerprint {
		t.Fatalf("retained checkpoint-ready = %+v", retainedParsed)
	}
	if changedParsed.fingerprint == parsed.fingerprint {
		t.Fatal("changed source cleanup proof retained checkpoint-ready fingerprint")
	}
}

func TestParseCheckpointReadyRequestRejectsInvalidSourceCleanup(t *testing.T) {
	for _, mutate := range []func(*workerapi.CheckpointReadyRequest){
		func(request *workerapi.CheckpointReadyRequest) {
			request.SourceCleanup.Method = workerapi.RuntimeCleanupHostReconciled
		},
		func(request *workerapi.CheckpointReadyRequest) { request.SourceCleanup.CompletedAt = time.Time{} },
	} {
		request := validCheckpointReadyRequest()
		mutate(&request)
		if _, _, err := parseCheckpointReadyRequest(request); err == nil || !strings.Contains(err.Error(), "source_cleanup") {
			t.Fatalf("err = %v, want source_cleanup rejection", err)
		}
	}
}

func TestValidateCheckpointSourceDisposition(t *testing.T) {
	cleanup := &workerapi.RuntimeCleanupProof{Method: workerapi.RuntimeCleanupSessionClosed}
	tests := []struct {
		name       string
		wait       db.RunWait
		cleanup    *workerapi.RuntimeCleanupProof
		wantRetain bool
		wantErr    bool
	}{
		{name: "ordinary closed", wait: db.RunWait{Kind: db.WaitKindToken}, cleanup: cleanup},
		{name: "ordinary missing proof", wait: db.RunWait{Kind: db.WaitKindToken}, wantErr: true},
		{name: "same workspace retained", wait: db.RunWait{Kind: db.WaitKindChild}, wantRetain: true},
		{name: "same workspace rejects proof", wait: db.RunWait{Kind: db.WaitKindChild}, cleanup: cleanup, wantErr: true},
		{name: "different workspace closed", wait: db.RunWait{Kind: db.WaitKindChild, ChildRunID: pgtype.UUID{Valid: true}}, cleanup: cleanup},
		{name: "different workspace missing proof", wait: db.RunWait{Kind: db.WaitKindChild, ChildRunID: pgtype.UUID{Valid: true}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retained, err := validateCheckpointSourceDisposition(tt.wait, tt.cleanup)
			if retained != tt.wantRetain || (err != nil) != tt.wantErr {
				t.Fatalf("disposition = retained:%v err:%v", retained, err)
			}
		})
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
		actor:   db.Session{State: "open"},
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
		RuntimeInstanceID: uuid.Must(uuid.NewV7()).String(), RuntimeIdentityID: runtimeIdentity,
		WorkspaceID: uuid.Must(uuid.NewV7()).String(), WorkspaceMountID: uuid.Must(uuid.NewV7()).String(),
		WorkspaceLeaseID: uuid.Must(uuid.NewV7()).String(), BaseWorkspaceVersionID: uuid.Must(uuid.NewV7()).String(),
		OwnershipGeneration: 1, WriterGeneration: 1, MountFencingGeneration: 1,
		RequestedCPUMillis: 1000, RequestedMemoryBytes: 1 << 30, RequestedExecutionSlots: 1,
		MaxActiveDurationMs: 60_000, StartDeadlineAt: time.Now().UTC().Add(-time.Minute),
		ExpiresAt: time.Now().UTC().Add(time.Minute),
	}
	return workerapi.CheckpointReadyRequest{
		Lease: lease.Fence(), RequestVersion: 1, RunWaitID: waitID, CheckpointID: checkpointID,
		SourceCleanup: &workerapi.RuntimeCleanupProof{
			Method: workerapi.RuntimeCleanupSessionClosed, CompletedAt: time.Now().UTC(),
		},
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
					Contract: "helmr.vm-runtime.v0", KernelDigest: digestWith("4"),
					InitramfsDigest: digestWith("5"), RootfsDigest: digestWith("6"), ConfigDigest: digestWith("7"),
					VMVCPUCount: 2, CPUConfigDigest: digestWith("8"),
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

func TestCheckpointRuntimeShapeAuthorityMatchesLockedRuntime(t *testing.T) {
	manifest := validCheckpointReadyRequest().Manifest
	runtime := db.RuntimeInstance{
		VMVCPUCount:     manifest.RecoveryPoint.Runtime.VMVCPUCount,
		CPUConfigDigest: manifest.RecoveryPoint.Runtime.CPUConfigDigest,
	}
	if err := validateCheckpointRuntimeShapeAuthority(runtime, manifest); err != nil {
		t.Fatal(err)
	}
	manifest.RecoveryPoint.Runtime.CPUConfigDigest = digestWith("9")
	if err := validateCheckpointRuntimeShapeAuthority(runtime, manifest); !errors.Is(err, errStaleRunLeaseClaim) {
		t.Fatalf("error = %v, want stale runtime shape", err)
	}
}

func digestWith(character string) string {
	return "sha256:" + strings.Repeat(character, 64)
}
