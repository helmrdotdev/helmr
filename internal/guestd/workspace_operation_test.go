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

	"github.com/helmrdotdev/helmr/internal/frameio"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func testComputerMountTarget(versionID string) *workspacev0.ComputerMountTarget {
	return &workspacev0.ComputerMountTarget{
		BaseWorkspaceVersionId: versionID,
	}
}

func TestRestoredComputerRebindPreservesPairedFilesystem(t *testing.T) {
	tempRoot := t.TempDir()
	liveRoot := filepath.Join(tempRoot, "live")
	if err := os.MkdirAll(liveRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveRoot, "retained.txt"), []byte("paired"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope: &workspacev0.WorkspaceOperationEnvelope{
			WorkspaceMountId: "mount-c", WorkspaceId: "workspace-1",
			ChannelToken: "channel-c", FencingGeneration: 2,
		},
		MountPath: "/workspace", Target: testComputerMountTarget("version-c"),
		UsePreparedRuntime: true, RuntimeInstanceId: "runtime-c",
		RestoredCheckpointId: "checkpoint-b", RestoreSourceVersionId: "version-a",
	}
	entry := &workspaceMountEntry{
		workspaceID: "workspace-1", workspaceMountID: "mount-b", channelToken: "channel-b",
		runtimeInstanceID: "runtime-b", workspaceMount: "/workspace", workspaceRoot: liveRoot,
		baseWorkspaceVersionID: "version-a", finalizationRoot: filepath.Join(tempRoot, "state"),
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
	if _, err := waits.registerProgram(&programv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 1, RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-b", ResumeAttachId: "resume-b", CheckpointRequestVersion: 1,
	}); err != nil {
		t.Fatal(err)
	}
	phases, err := registry.materializeRestoredWorkspaceMount(request, waits)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].GetName() != "guest_restore_materialize" {
		t.Fatalf("phases = %+v", phases)
	}
	if content, err := os.ReadFile(filepath.Join(liveRoot, "retained.txt")); err != nil || string(content) != "paired" {
		t.Fatalf("paired file = %q, %v", content, err)
	}
	if registry.entries["mount-b"] != nil || registry.entries["mount-c"] != entry ||
		entry.baseWorkspaceVersionID != "version-c" || entry.currentFencingGeneration() != 2 {
		t.Fatalf("rebinding state = %+v", entry)
	}
	if replay, err := registry.materializeRestoredWorkspaceMount(request, waits); err != nil ||
		len(replay) != 1 || replay[0].GetName() != "guest_restore_materialize_replay" {
		t.Fatalf("replay = %+v, %v", replay, err)
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
	if response.GetStatus() != "prepared" || response.GetRuntimeInstanceId() != runtimeInstanceID {
		t.Fatalf("response state=%q runtime_instance_id=%q", response.GetStatus(), response.GetRuntimeInstanceId())
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
			_, _, err = restoreWorkspaceMount(&workspacev0.MaterializeWorkspaceRequest{
				Envelope:          &workspacev0.WorkspaceOperationEnvelope{},
				MountPath:         "/workspace",
				Target:            testComputerMountTarget("version-1"),
				RuntimeInstanceId: "runtime-instance-1",
				WorkspaceImage:    workspaceImage,
			}, newWorkspaceOperationRegistry())
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
		Target:            testComputerMountTarget("version-1"),
		RuntimeInstanceId: "runtime-instance-1",
	}); err != nil {
		t.Fatal(err)
	}
	var response workspacev0.MaterializeWorkspaceResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "failed" {
		t.Fatalf("response state = %q, want failed", response.Status)
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

func TestPreparedImageConfigRequiresMountedSubstrate(t *testing.T) {
	t.Setenv(guestdSubstrateRootEnv, "")
	_, cleanup, err := restorePreparedWorkspaceImage(strings.NewReader(""), &workspacev0.PrepareWorkspaceRuntimeRequest{MountedImageConfig: &workspacev0.RuntimeImageConfig{}})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "requires a mounted substrate") {
		t.Fatalf("error = %v", err)
	}
}

func TestPreparedComputerMountPreservesFilesAndSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "project.txt")
	if err := os.WriteFile(path, []byte("persisted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("project.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.setPreparedRuntime(&preparedWorkspaceRuntime{runtimeInstanceID: "runtime", workspaceImageDigest: "image", workspaceMount: "/workspace", imageRoot: root, workspaceRoot: root, cleanup: func() {}})
	entry, _, err := restoreWorkspaceMount(&workspacev0.MaterializeWorkspaceRequest{Envelope: &workspacev0.WorkspaceOperationEnvelope{WorkspaceMountId: "mount"}, MountPath: "/workspace", Target: testComputerMountTarget("version"), RuntimeInstanceId: "runtime", UsePreparedRuntime: true, WorkspaceImage: &workspacev0.WorkspaceArtifact{Digest: "image", MediaType: workspaceImageMediaType, Encoding: workspaceImageEncoding, SizeBytes: 1}}, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer entry.cleanup()
	if got, err := os.ReadFile(path); err != nil || string(got) != "persisted" {
		t.Fatalf("file=%q %v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(root, "link")); err != nil || got != "project.txt" {
		t.Fatalf("symlink=%q %v", got, err)
	}
}
