package guestd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	runv0 "github.com/helmrdotdev/helmr/internal/proto/run/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"github.com/helmrdotdev/helmr/internal/workspace"
)

func testWorkspaceArtifactTarget(
	t *testing.T,
	versionID string,
	artifact workspace.WorkspaceArtifact,
) *workspacev0.WorkspaceResetTarget {
	t.Helper()
	file, err := os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	tree, inspectErr := workspace.InspectArtifact(file, artifact)
	closeErr := file.Close()
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	return &workspacev0.WorkspaceResetTarget{
		BaseVersionId: versionID,
		Tree: &workspacev0.WorkspaceTreeIdentity{
			Digest: tree.Digest, SizeBytes: tree.SizeBytes, EntryCount: uint32(tree.EntryCount),
		},
		Source: &workspacev0.WorkspaceResetTarget_Artifact{Artifact: &workspacev0.WorkspaceArtifact{
			Digest: artifact.Digest, MediaType: artifact.MediaType, Encoding: artifact.Encoding,
			SizeBytes: uint64(artifact.SizeBytes), EntryCount: uint32(artifact.EntryCount),
		}},
	}
}

func testEmptyWorkspaceTarget(versionID string) *workspacev0.WorkspaceResetTarget {
	return &workspacev0.WorkspaceResetTarget{
		BaseVersionId: versionID,
		Tree:          &workspacev0.WorkspaceTreeIdentity{Digest: workspace.CanonicalEmptyTreeDigest},
		Source:        &workspacev0.WorkspaceResetTarget_Empty{Empty: &workspacev0.EmptyWorkspaceResetTarget{}},
	}
}

func TestRestoredWorkspaceMaterializesExactTargetBeforeRebindingAuthority(t *testing.T) {
	tempRoot := t.TempDir()
	liveRoot := filepath.Join(tempRoot, "live")
	targetRoot := filepath.Join(tempRoot, "target")
	if err := os.MkdirAll(liveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, "from-b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(targetRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetRoot, "from-c.txt"), []byte("C"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifact, cleanup, err := workspace.CreateWorkspaceArtifactFromRoot(targetRoot, tempRoot, tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId: "mount-c", WorkspaceId: "workspace-1",
			ChannelToken: "channel-c", FencingGeneration: 2,
		},
		MountPath: "/workspace", Target: testWorkspaceArtifactTarget(t, "version-c", artifact),
		UsePreparedRuntime: true, RuntimeInstanceId: "runtime-c",
		RestoredCheckpointId: "checkpoint-b", RestoreSourceVersionId: "version-a",
	}
	entry := &workspaceMountEntry{
		workspaceID: "workspace-1", workspaceMountID: "mount-b", channelToken: "channel-b",
		runtimeInstanceID: "runtime-b", workspaceMount: "/workspace", workspaceRoot: liveRoot,
		baseVersionID: "version-a", finalizationRoot: filepath.Join(tempRoot, "state"),
		authorityState: workspaceAuthorityLive,
	}
	entry.setFencingGeneration(1)
	registry := newWorkspaceOperationRegistry()
	registry.entries["mount-b"] = entry
	registry.programClaims = []*managedProgramClaim{{
		entry: entry,
		authority: &workspacev0.WorkspaceRunAuthority{Fence: &workspacev0.WorkspaceAuthorityFence{
			RunId: "run-1", AttemptNumber: 1, RunLeaseId: "lease-b",
		}},
		released: make(chan struct{}),
	}}
	waits := newWaitingRunRegistry()
	if _, err := waits.registerProgram(&runv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 1, RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-b", ResumeAttachId: "resume-b", CheckpointRequestVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	artifactFrame := func() *bytes.Buffer {
		var stream bytes.Buffer
		if err := wire.WriteFileFrame(&stream, wire.StreamHeader{
			Type: wire.StreamTypeWorkspaceArtifact, WorkspaceID: "workspace-1",
		}, artifact.Path); err != nil {
			t.Fatal(err)
		}
		return &stream
	}
	phases, err := registry.materializeRestoredWorkspaceMount(artifactFrame(), request, waits)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].GetName() != "guest_restore_materialize" {
		t.Fatalf("phases = %+v", phases)
	}
	if _, err := os.Stat(filepath.Join(liveRoot, "from-b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("B-only path remains: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(liveRoot, "from-c.txt")); err != nil || string(content) != "C" {
		t.Fatalf("C path = %q, %v", content, err)
	}
	if registry.entries["mount-b"] != nil || registry.entries["mount-c"] != entry ||
		entry.baseVersionID != "version-c" || entry.currentFencingGeneration() != 2 {
		t.Fatalf("rebinding state = %+v", entry)
	}
	if replay, err := registry.materializeRestoredWorkspaceMount(artifactFrame(), request, waits); err != nil ||
		len(replay) != 1 || replay[0].GetName() != "guest_restore_materialize_replay" {
		t.Fatalf("replay = %+v, %v", replay, err)
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
		MountPath: mountPath,
		Target:    testWorkspaceArtifactTarget(t, "version-1", artifact),
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
				Target:            testEmptyWorkspaceTarget("version-1"),
				RuntimeInstanceId: "runtime-instance-1",
				WorkspaceImage:    workspaceImage,
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
		MountPath: "/workspace",
		Target:    testEmptyWorkspaceTarget("version-zero"),
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
		Target:            testEmptyWorkspaceTarget("version-1"),
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
	if response.Target != nil {
		t.Fatalf("failed response target = %+v, want nil", response.Target)
	}
	if response.GuestdChannelTokenHash != "" {
		t.Fatalf("failed response channel token hash = %q, want empty", response.GuestdChannelTokenHash)
	}
	if got := testWorkspaceMountPhaseError(response.Phases); !strings.Contains(got, "mount_path") {
		t.Fatalf("phase error = %q, want mount_path", got)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "mount_path") {
		t.Fatalf("handler error = %v, want mount_path", err)
	}
}

func TestWorkspaceStopCaptureReportsTreeDerivedFromArtifact(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "marker.txt"), []byte("captured marker"), 0o640); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.register("mat-1", &workspaceMountEntry{
		workspaceID: "workspace-1", workspaceMountID: "mat-1", channelToken: "token-1",
		fencingGeneration: 1, workspaceRoot: root, cleanup: func() {},
	})
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- handleWorkspaceStopConnection(context.Background(), server, registry)
	}()
	if err := frameio.WriteProtoFrame(client, &workspacev0.StopWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId: "mat-1", WorkspaceId: "workspace-1", ChannelToken: "token-1", FencingGeneration: 1,
		},
		CaptureBeforeStop: true,
	}); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.StopWorkspaceResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetState() != "captured" || response.GetCapturedTree() == nil || response.GetCapturedArtifact() == nil {
		t.Fatalf("capture response = %+v", &response)
	}
	header, bodyLen, err := wire.ReadStreamFrameHeader(client)
	if err != nil {
		t.Fatal(err)
	}
	if header.Type != wire.StreamTypeWorkspaceArtifact || header.WorkspaceID != "workspace-1" {
		t.Fatalf("artifact header = %+v", header)
	}
	body, err := io.ReadAll(&io.LimitedReader{R: client, N: int64(bodyLen)})
	if err != nil {
		t.Fatal(err)
	}
	tree := workspace.TreeIdentity{
		Digest: response.GetCapturedTree().GetDigest(), SizeBytes: response.GetCapturedTree().GetSizeBytes(),
		EntryCount: int(response.GetCapturedTree().GetEntryCount()),
	}
	artifact := workspace.WorkspaceArtifact{
		Digest: response.GetCapturedArtifact().GetDigest(), MediaType: response.GetCapturedArtifact().GetMediaType(),
		Encoding: response.GetCapturedArtifact().GetEncoding(), SizeBytes: int64(response.GetCapturedArtifact().GetSizeBytes()),
		EntryCount: int(response.GetCapturedArtifact().GetEntryCount()),
	}
	if tree.Digest == artifact.Digest || tree.SizeBytes == artifact.SizeBytes {
		t.Fatalf("tree and artifact identities were conflated: tree=%+v artifact=%+v", tree, artifact)
	}
	if err := workspace.VerifyArtifact(bytes.NewReader(body), artifact, tree); err != nil {
		t.Fatalf("captured tree is not derived from exact artifact: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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
