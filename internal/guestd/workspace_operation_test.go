package guestd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"google.golang.org/protobuf/proto"
)

func TestManagedProgramChildAdmissionPreservesParentClaim(t *testing.T) {
	entry, registry, parent := testWorkspaceFinalizationMountUnadmitted(t)
	releaseParent, err := registry.admitProgram(entry, parent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseParent()
	if err := registry.authorizeChildProgram(
		entry,
		parent.GetFence().GetRunId(),
		parent.GetFence().GetAttemptNumber(),
	); err != nil {
		t.Fatal(err)
	}
	child := proto.Clone(parent).(*workspacev0.WorkspaceRunAuthority)
	child.GetFence().RunId = "run-child"
	child.GetFence().RunLeaseId = "run-lease-child"
	child.GetFence().WorkspaceLeaseId = "workspace-lease-child"
	child.GetFence().WriterGeneration++
	child.GetFence().MountFencingGeneration++
	child.GetFence().BaseWorkspaceVersionId = "captured-private-version"
	child.WriteCapability = "child-write-capability"
	releaseChild, err := registry.admitProgram(entry, child, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	registry.mu.Lock()
	if len(registry.programClaims) != 2 ||
		!workspaceRunAuthoritiesEqual(registry.programClaims[0].authority, parent) ||
		!workspaceRunAuthoritiesEqual(registry.programClaims[1].authority, child) {
		t.Fatalf("Program claims = %+v", registry.programClaims)
	}
	registry.mu.Unlock()
	entry.authorityMu.Lock()
	current := proto.Clone(entry.authority).(*workspacev0.WorkspaceRunAuthority)
	entry.authorityMu.Unlock()
	if !workspaceRunAuthoritiesEqual(current, child) ||
		entry.baseVersionID != "captured-private-version" {
		t.Fatal("child admission did not advance current Workspace authority")
	}
	waited := make(chan error, 1)
	go func() {
		waited <- registry.waitForProgramRelease(context.Background(), entry, child)
	}()
	select {
	case err := <-waited:
		t.Fatalf("child finalization did not wait for child release: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseChild()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("child finalization remained blocked by frozen parent")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if len(registry.programClaims) != 1 ||
		!workspaceRunAuthoritiesEqual(registry.programClaims[0].authority, parent) {
		t.Fatal("child release discarded the frozen parent claim")
	}
}

func TestManagedProgramChildAdmissionRejectsUnverifiedOrDifferentRuntime(t *testing.T) {
	entry, registry, parent := testWorkspaceFinalizationMountUnadmitted(t)
	releaseParent, err := registry.admitProgram(entry, parent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseParent()
	child := proto.Clone(parent).(*workspacev0.WorkspaceRunAuthority)
	child.GetFence().RunId = "run-child"
	child.GetFence().RunLeaseId = "run-lease-child"
	child.GetFence().WorkspaceLeaseId = "workspace-lease-child"
	child.GetFence().WriterGeneration++
	child.GetFence().MountFencingGeneration++
	child.GetFence().BaseWorkspaceVersionId = "captured-private-version"
	if _, err := registry.admitProgram(entry, child, time.Now()); err == nil {
		t.Fatal("unverified child Program was admitted")
	}
	if err := registry.authorizeChildProgram(
		entry,
		parent.GetFence().GetRunId(),
		parent.GetFence().GetAttemptNumber(),
	); err != nil {
		t.Fatal(err)
	}
	child.GetFence().RuntimeInstanceId = "different-runtime"
	if _, err := registry.admitProgram(entry, child, time.Now()); err == nil {
		t.Fatal("child Program on a different runtime was admitted")
	}
	if entry.baseVersionID != parent.GetFence().GetBaseWorkspaceVersionId() ||
		entry.currentFencingGeneration() != uint64(parent.GetFence().GetMountFencingGeneration()) {
		t.Fatal("failed child admission mutated mounted Workspace authority")
	}
}

func TestManagedProgramChildAdmissionRequiresCapturedBaseAdvance(t *testing.T) {
	entry, registry, parent := testWorkspaceFinalizationMountUnadmitted(t)
	releaseParent, err := registry.admitProgram(entry, parent, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer releaseParent()
	if err := registry.authorizeChildProgram(
		entry,
		parent.GetFence().GetRunId(),
		parent.GetFence().GetAttemptNumber(),
	); err != nil {
		t.Fatal(err)
	}
	child := proto.Clone(parent).(*workspacev0.WorkspaceRunAuthority)
	child.GetFence().RunId = "run-child"
	child.GetFence().RunLeaseId = "run-lease-child"
	child.GetFence().WorkspaceLeaseId = "workspace-lease-child"
	child.GetFence().WriterGeneration++
	child.GetFence().MountFencingGeneration++
	child.WriteCapability = "child-write-capability"
	if _, err := registry.admitProgram(entry, child, time.Now()); err == nil {
		t.Fatal("child Program without a captured base advance was admitted")
	}
	if entry.baseVersionID != parent.GetFence().GetBaseWorkspaceVersionId() ||
		entry.currentFencingGeneration() != uint64(parent.GetFence().GetMountFencingGeneration()) {
		t.Fatal("rejected child mutated mounted Workspace authority")
	}
}

func TestRestoredWorkspaceRebindPreservesFrozenProgramAndReplacesMountAuthority(t *testing.T) {
	registry := newWorkspaceOperationRegistry()
	entry := &workspaceMountEntry{
		workspaceID: "workspace-1", workspaceMountID: "old-mount", channelToken: "old-channel",
		runtimeInstanceID: "old-runtime", workspaceMount: "/workspace", baseVersionID: "old-version",
		authority:      &workspacev0.WorkspaceRunAuthority{Fence: &workspacev0.WorkspaceAuthorityFence{RunLeaseId: "old-lease"}},
		authorityState: workspaceAuthorityLive,
	}
	entry.setFencingGeneration(3)
	registry.entries["old-mount"] = entry
	registry.programClaims = []*managedProgramClaim{{
		entry: entry,
		authority: &workspacev0.WorkspaceRunAuthority{Fence: &workspacev0.WorkspaceAuthorityFence{
			RunId: "run-1", AttemptNumber: 1, RunLeaseId: "old-lease",
		}},
		released: make(chan struct{}),
	}}
	waits := newWaitingRunRegistry()
	if _, err := waits.registerProgram(&runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 1, RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-1", ResumeAttachId: "attach-1", CheckpointRequestVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId: "new-mount", WorkspaceId: "workspace-1",
			ChannelToken: "new-channel", FencingGeneration: 4,
		},
		MountPath: "/workspace", BaseVersionId: "private-version", UsePreparedRuntime: true,
		RuntimeInstanceId: "new-runtime", RestoredCheckpointId: "checkpoint-1",
	}
	phases, err := registry.rebindRestoredWorkspaceMount(request, waits)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || registry.entries["old-mount"] != nil || registry.entries["new-mount"] != entry ||
		len(registry.programClaims) != 1 || registry.programClaims[0].entry != entry || entry.authority != nil ||
		entry.workspaceMountID != "new-mount" || entry.channelToken != "new-channel" ||
		entry.runtimeInstanceID != "new-runtime" || entry.baseVersionID != "private-version" ||
		entry.currentFencingGeneration() != 4 {
		t.Fatalf("restored rebind state = entry=%+v phases=%+v", entry, phases)
	}
	if replay, err := registry.rebindRestoredWorkspaceMount(request, waits); err != nil ||
		len(replay) != 1 || replay[0].GetName() != "guest_restore_rebind_replay" {
		t.Fatalf("restored rebind replay = %+v, %v", replay, err)
	}
	request.RestoredCheckpointId = "different-checkpoint"
	if _, err := registry.rebindRestoredWorkspaceMount(request, waits); err == nil {
		t.Fatal("mismatched restored Checkpoint was accepted")
	}
}

func TestWorkspaceMaterializeRestoresArtifactAndAuthorizesPrimitiveOperation(t *testing.T) {
	tempRoot := t.TempDir()
	root := filepath.Join(tempRoot, "workspace-root")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(root, tempRoot, tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256sum.DigestBytes(image)
	mountPath := "/workspace"
	registry := newWorkspaceOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry, nil)
	}()
	if err := frameio.WriteProtoFrame(materializeClient, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-1",
			WorkspaceId:       "workspace-1",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:     mountPath,
		BaseVersionId: "version-1",
		BaseArtifact: &workspacev0.WorkspaceArtifact{
			Digest:     artifact.Digest,
			MediaType:  artifact.MediaType,
			Encoding:   artifact.Encoding,
			SizeBytes:  uint64(artifact.SizeBytes),
			EntryCount: uint32(artifact.EntryCount),
		},
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    imageDigest,
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
		RuntimeInstanceId: "runtime-instance-1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(materializeClient, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: "workspace-1",
	}, imagePath); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(materializeClient, wire.StreamHeader{
		Type:        wire.StreamTypeWorkspaceArtifact,
		WorkspaceID: "workspace-1",
	}, artifact.Path); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "running" || response.GuestdChannelTokenHash != sha256sum.HexBytes([]byte("channel-token")) {
		t.Fatalf("response state=%q guestd_channel_token_hash=%q", response.State, response.GuestdChannelTokenHash)
	}
	if !workspaceMountPhaseNames(response.Phases, "guest_workspace_image_restore", "guest_workspace_artifact_restore", "guest_register") {
		t.Fatalf("response phases = %+v", response.Phases)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	entry := registry.entries["mat-1"]
	workspaceRoot := entry.workspaceRoot
	registry.mu.RUnlock()
	stateRelativeToImage, err := filepath.Rel(entry.imageRoot, entry.finalizationRoot)
	if err != nil {
		t.Fatal(err)
	}
	if stateRelativeToImage != ".." && !strings.HasPrefix(stateRelativeToImage, ".."+string(os.PathSeparator)) {
		t.Fatalf("Workspace finalization state %q is visible inside image root %q", entry.finalizationRoot, entry.imageRoot)
	}
	if tempRoot := os.Getenv("HELMR_GUESTD_TMPDIR"); tempRoot != "" && !strings.HasPrefix(workspaceRoot, tempRoot+string(os.PathSeparator)) {
		t.Fatalf("workspace root = %q, want under HELMR_GUESTD_TMPDIR %q", workspaceRoot, tempRoot)
	}
	body, err := os.ReadFile(filepath.Join(workspaceRoot, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello" {
		t.Fatalf("restored file = %q", body)
	}
}

func TestWorkspaceRuntimePrepareUsesWorkspaceImageAndRuntimeInstanceID(t *testing.T) {
	tempRoot := t.TempDir()
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256sum.DigestBytes(image)
	registry := newWorkspaceOperationRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceRuntimePrepareConnection(context.Background(), server, slogDiscard(), registry)
	}()
	const runtimeInstanceID = " runtime-instance-1 "
	if err := frameio.WriteProtoFrame(client, &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeInstanceId: runtimeInstanceID,
		MountPath:         "/workspace",
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    imageDigest,
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(client, wire.StreamHeader{Type: wire.StreamTypeRunImage}, imagePath); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.PrepareWorkspaceRuntimeResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetState() != "prepared" || response.GetRuntimeInstanceId() != runtimeInstanceID {
		t.Fatalf("response state=%q runtime_instance_id=%q", response.GetState(), response.GetRuntimeInstanceId())
	}
	if !workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_image_restore", "guest_runtime_user_resolve", "guest_workspace_root_resolve") {
		t.Fatalf("response phases = %+v", response.GetPhases())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.takePreparedRuntime("runtime-instance-other", imageDigest, "/workspace"); ok {
		t.Fatal("prepared runtime accepted a different runtime_instance_id")
	}
	if _, ok := registry.takePreparedRuntime(strings.TrimSpace(runtimeInstanceID), imageDigest, "/workspace"); ok {
		t.Fatal("prepared runtime normalized an opaque runtime_instance_id")
	}
	prepared, ok := registry.takePreparedRuntime(runtimeInstanceID, imageDigest, "/workspace")
	if !ok {
		t.Fatal("prepared runtime did not accept the matching runtime_instance_id")
	}
	prepared.cleanup()
}

func TestWorkspaceImageContractIsExact(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		encoding  string
		want      string
	}{
		{name: "padded media type", mediaType: " " + workspaceImageMediaType, encoding: workspaceImageEncoding, want: "media_type"},
		{name: "wrong encoding", mediaType: workspaceImageMediaType, encoding: "tar", want: "encoding"},
		{name: "padded encoding", mediaType: workspaceImageMediaType, encoding: workspaceImageEncoding + " ", want: "encoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspaceImage := &workspacev0.WorkspaceArtifact{
				Digest:    "sha256:image",
				MediaType: tt.mediaType,
				Encoding:  tt.encoding,
				SizeBytes: 1,
			}
			_, _, err := restorePreparedWorkspaceRuntime(bytes.NewReader(nil), &workspacev0.PrepareWorkspaceRuntimeRequest{
				RuntimeInstanceId: "runtime-instance-1",
				MountPath:         "/workspace",
				WorkspaceImage:    workspaceImage,
			}, slogDiscard())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prepare error = %v, want %s rejection", err, tt.want)
			}
			_, _, err = restoreWorkspaceMount(bytes.NewReader(nil), &workspacev0.MaterializeWorkspaceRequest{
				Envelope:          &workspacev0.WorkspaceOperationEnvelope{},
				MountPath:         "/workspace",
				BaseVersionId:     "version-1",
				RuntimeInstanceId: "runtime-instance-1",
				BaseArtifact: &workspacev0.WorkspaceArtifact{
					Digest:     "sha256:base",
					MediaType:  workspace.ArtifactMediaType,
					Encoding:   workspace.ArtifactEncoding,
					SizeBytes:  1,
					EntryCount: 0,
				},
				WorkspaceImage: workspaceImage,
			}, slogDiscard(), newWorkspaceOperationRegistry())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("materialize error = %v, want %s rejection", err, tt.want)
			}
		})
	}
	_, _, err := restorePreparedWorkspaceRuntime(bytes.NewReader(nil), &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeInstanceId: "   ",
		MountPath:         "/workspace",
	}, slogDiscard())
	if err == nil || !strings.Contains(err.Error(), "runtime_instance_id is required") {
		t.Fatalf("whitespace runtime_instance_id error = %v", err)
	}
}

func TestWorkspaceMaterializeWithoutBaseArtifactInitializesEmptyRoot(t *testing.T) {
	tempRoot := t.TempDir()
	image := ociTar(t, []ociTestLayer{{
		mediaType: "application/vnd.oci.image.layer.v1.tar",
		body:      tarBytes(t, map[string]string{"workspace/from-image.txt": "remove me"}),
	}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "workspace-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), server, slogDiscard(), registry, nil)
	}()
	if err := frameio.WriteProtoFrame(client, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-empty",
			WorkspaceId:       "workspace-empty",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:     "/workspace",
		BaseVersionId: "version-zero",
		WorkspaceImage: &workspacev0.WorkspaceArtifact{
			Digest:    sha256sum.DigestBytes(image),
			MediaType: workspaceImageMediaType,
			Encoding:  workspaceImageEncoding,
			SizeBytes: uint64(len(image)),
		},
		RuntimeInstanceId: "runtime-instance-empty",
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(client, wire.StreamHeader{
		Type:        wire.StreamTypeRunImage,
		WorkspaceID: "workspace-empty",
	}, imagePath); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetState() != "running" {
		t.Fatalf("response state = %q", response.GetState())
	}
	if !workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_image_restore", "guest_workspace_empty_root_init", "guest_register") {
		t.Fatalf("response phases = %+v", response.GetPhases())
	}
	if workspaceMountPhaseNames(response.GetPhases(), "guest_workspace_artifact_restore") {
		t.Fatalf("base-artifact-free response unexpectedly restored an artifact: %+v", response.GetPhases())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	registry.mu.RLock()
	workspaceRoot := registry.entries["mat-empty"].workspaceRoot
	registry.mu.RUnlock()
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty workspace root entries = %v", entries)
	}
}

func TestWorkspaceMaterializeReturnsFailureResponse(t *testing.T) {
	registry := newWorkspaceOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleWorkspaceMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry, nil)
	}()
	if err := frameio.WriteProtoFrame(materializeClient, &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId:  "mat-1",
			WorkspaceId:       "workspace-1",
			ChannelToken:      "channel-token",
			FencingGeneration: 1,
		},
		MountPath:         "relative",
		BaseVersionId:     "version-1",
		RuntimeInstanceId: "runtime-instance-1",
	}); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.State != "failed" {
		t.Fatalf("response state = %q, want failed", response.State)
	}
	if got := testWorkspaceMountPhaseError(response.Phases); !strings.Contains(got, "mount_path") {
		t.Fatalf("phase error = %q, want mount_path", got)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "mount_path") {
		t.Fatalf("handler error = %v, want mount_path", err)
	}
}

func testWorkspaceMountPhaseError(phases []*workspacev0.WorkspaceMountPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i] != nil && strings.TrimSpace(phases[i].GetError()) != "" {
			return strings.TrimSpace(phases[i].GetError())
		}
	}
	return ""
}

func workspaceMountPhaseNames(phases []*workspacev0.WorkspaceMountPhase, expected ...string) bool {
	seen := map[string]bool{}
	for _, phase := range phases {
		if phase == nil {
			continue
		}
		seen[phase.Name] = true
	}
	for _, name := range expected {
		if !seen[name] {
			return false
		}
	}
	return true
}

func TestWorkspaceOperationRegistryDefersRetiredCleanupUntilRelease(t *testing.T) {
	tempRoot := t.TempDir()
	oldRoot := filepath.Join(tempRoot, "old")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", &workspaceMountEntry{
		workspaceID:       "workspace-1",
		channelToken:      "token-1",
		fencingGeneration: 1,
		workspaceRoot:     filepath.Join(oldRoot, "workspace"),
		cleanup:           func() { _ = os.RemoveAll(oldRoot) },
	})
	_, release, ok := registry.acquire("mat-1", "workspace-1", "token-1", 1)
	if !ok {
		t.Fatal("expected registry acquire")
	}
	registry.register("mat-1", &workspaceMountEntry{
		workspaceID:       "workspace-1",
		channelToken:      "token-2",
		fencingGeneration: 2,
		workspaceRoot:     filepath.Join(tempRoot, "new", "workspace"),
		cleanup:           func() {},
	})
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatalf("old workspace root was cleaned while acquired: %v", err)
	}
	release()
	if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old workspace root after release err = %v, want not exist", err)
	}
}
