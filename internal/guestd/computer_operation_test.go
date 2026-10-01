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
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	programv0 "github.com/helmrdotdev/helmr/internal/proto/program/v0"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

func testComputerMountTarget(versionID string) *computerv0.ComputerMountTarget {
	return &computerv0.ComputerMountTarget{
		BaseComputerDiskVersionId: versionID,
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
	request := &computerv0.MaterializeComputerRequest{
		Envelope: &computerv0.ComputerOperationEnvelope{
			ComputerInstanceId: "runtime-c", ComputerId: "computer-1",
			ChannelCredential: "channel-c", WriterGeneration: 2,
		},
		MountPath: "/workspace", Target: testComputerMountTarget("version-c"),
		UsePreparedRuntime:   true,
		RestoredCheckpointId: "checkpoint-b",
	}
	entry := &computerMountEntry{
		computerID: "computer-1", channelCredential: "channel-b",
		computerInstanceID: "runtime-b", computerMount: "/workspace", computerRoot: liveRoot,
		baseComputerDiskVersionID: "version-a",
	}
	entry.setWriterGeneration(1)
	registry := newComputerOperationRegistry()
	registry.entries["runtime-b"] = entry
	registry.programClaims = []*managedProgramClaim{{
		entry: entry,
		authority: &computerv0.ComputerRunAuthority{Fence: &computerv0.ComputerAuthorityFence{
			RunId: "run-1", AttemptNumber: 1, RunLeaseId: "lease-b", ComputerId: "computer-1", ComputerInstanceId: "runtime-b", WriterGeneration: 1,
		}},
	}}
	waits := newWaitingRunRegistry()
	registration, err := waits.registerProgram(&programv0.CheckpointPauseRequest{
		RunId: "run-1", AttemptNumber: 1, RunLeaseId: "lease-b", RunWaitId: "wait-1", CorrelationId: "correlation-1",
		CheckpointId: "checkpoint-b", ResumeAttachId: "resume-b", CheckpointRequestVersion: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	registration.markFrozen()
	registry.captureRequest = &computerv0.FreezeComputerRequest{ComputerId: "computer-1", ComputerInstanceId: "runtime-b", WriterGeneration: 1, CheckpointId: "checkpoint-b", Runs: []*computerv0.ComputerCaptureRun{{RunId: "run-1", AttemptNumber: 1, RunLeaseId: "lease-b", RunWaitId: "wait-1"}}}
	invalid := proto.Clone(request).(*computerv0.MaterializeComputerRequest)
	invalid.Envelope.ComputerInstanceId = entry.computerInstanceID
	if _, err := registry.materializeRestoredComputerMount(invalid, waits); err == nil {
		t.Fatal("restore reused the source Instance identity")
	}
	if entry.currentWriterGeneration() != 1 || entry.baseComputerDiskVersionID != "version-a" {
		t.Fatal("rejected restore changed source authority")
	}
	phases, err := registry.materializeRestoredComputerMount(request, waits)
	if err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0].GetName() != "guest_restore_materialize" {
		t.Fatalf("phases = %+v", phases)
	}
	if content, err := os.ReadFile(filepath.Join(liveRoot, "retained.txt")); err != nil || string(content) != "paired" {
		t.Fatalf("paired file = %q, %v", content, err)
	}
	if registry.entries["runtime-b"] != nil || registry.entries["runtime-c"] != entry ||
		entry.baseComputerDiskVersionID != "version-c" || entry.currentWriterGeneration() != 2 {
		t.Fatalf("rebinding state = %+v", entry)
	}
	if replay, err := registry.materializeRestoredComputerMount(request, waits); err != nil ||
		len(replay) != 1 || replay[0].GetName() != "guest_restore_materialize_replay" {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
}

func TestComputerRuntimePrepareUsesComputerImageAndComputerInstanceID(t *testing.T) {
	tempRoot := t.TempDir()
	image := ociTar(t, []ociTestLayer{{mediaType: "application/vnd.oci.image.layer.v1.tar", body: tarBytes(t, nil)}}, []byte(`{"Config":{}}`))
	imagePath := filepath.Join(tempRoot, "computer-image.oci.tar")
	if err := os.WriteFile(imagePath, image, 0o644); err != nil {
		t.Fatal(err)
	}
	imageDigest := sha256sum.DigestBytes(image)
	registry := newComputerOperationRegistry()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleComputerRuntimePrepareConnection(context.Background(), server, slogDiscard(), registry)
	}()
	const computerInstanceID = " computer-instance-1 "
	if err := frameio.WriteProtoFrame(client, &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2,
		ComputerInstanceId: computerInstanceID,
		MountPath:          "/workspace",
		ComputerImage: &computerv0.ComputerArtifact{
			Digest:    imageDigest,
			MediaType: computerImageMediaType,
			Encoding:  computerImageEncoding,
			SizeBytes: uint64(len(image)),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteFileFrame(client, wire.StreamHeader{Type: wire.StreamTypeRunImage}, imagePath); err != nil {
		t.Fatal(err)
	}
	var response computerv0.PrepareComputerRuntimeResponse
	if err := frameio.ReadProtoFrame(client, &response); err != nil {
		t.Fatal(err)
	}
	if response.GetStatus() != "prepared" || response.GetComputerInstanceId() != computerInstanceID {
		t.Fatalf("response state=%q computer_instance_id=%q", response.GetStatus(), response.GetComputerInstanceId())
	}
	if !computerMountPhaseNames(response.GetPhases(), "guest_computer_image_restore", "guest_runtime_user_resolve", "guest_computer_root_resolve") {
		t.Fatalf("response phases = %+v", response.GetPhases())
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.takePreparedRuntime("computer-instance-other", "computer-1", imageDigest, "/workspace", 2); ok {
		t.Fatal("prepared runtime accepted a different computer_instance_id")
	}
	if _, ok := registry.takePreparedRuntime(strings.TrimSpace(computerInstanceID), "computer-1", imageDigest, "/workspace", 2); ok {
		t.Fatal("prepared runtime normalized an opaque computer_instance_id")
	}
	prepared, ok := registry.takePreparedRuntime(computerInstanceID, "computer-1", imageDigest, "/workspace", 2)
	if !ok {
		t.Fatal("prepared runtime did not accept the matching computer_instance_id")
	}
	prepared.cleanup()
}

func TestComputerImagePreparationContractIsExact(t *testing.T) {
	tests := []struct {
		name      string
		mediaType string
		encoding  string
		want      string
	}{
		{name: "padded media type", mediaType: " " + computerImageMediaType, encoding: computerImageEncoding, want: "media_type"},
		{name: "wrong encoding", mediaType: computerImageMediaType, encoding: "tar", want: "encoding"},
		{name: "padded encoding", mediaType: computerImageMediaType, encoding: computerImageEncoding + " ", want: "encoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			computerImage := &computerv0.ComputerArtifact{
				Digest:    "sha256:image",
				MediaType: tt.mediaType,
				Encoding:  tt.encoding,
				SizeBytes: 1,
			}
			_, _, err := restorePreparedComputerRuntime(bytes.NewReader(nil), &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2,
				ComputerInstanceId: "computer-instance-1",
				MountPath:          "/workspace",
				ComputerImage:      computerImage,
			}, slogDiscard())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("prepare error = %v, want %s rejection", err, tt.want)
			}
		})
	}
	_, _, err := restorePreparedComputerRuntime(bytes.NewReader(nil), &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2,
		ComputerInstanceId: "   ",
		MountPath:          "/workspace",
	}, slogDiscard())
	if err == nil || !strings.Contains(err.Error(), "identity is required") {
		t.Fatalf("whitespace computer_instance_id error = %v", err)
	}
}

func TestComputerMaterializeReturnsFailureResponse(t *testing.T) {
	registry := newComputerOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleComputerMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry, nil)
	}()
	if err := frameio.WriteProtoFrame(materializeClient, &computerv0.MaterializeComputerRequest{
		Envelope: &computerv0.ComputerOperationEnvelope{
			ComputerInstanceId: "computer-instance-1", ComputerId: "computer-1",
			ChannelCredential: "channel-credential",
			WriterGeneration:  1,
		},
		MountPath: "relative",
		Target:    testComputerMountTarget("version-1"),
	}); err != nil {
		t.Fatal(err)
	}
	var response computerv0.MaterializeComputerResponse
	if err := frameio.ReadProtoFrame(materializeClient, &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "failed" {
		t.Fatalf("response state = %q, want failed", response.Status)
	}
	if response.Target != nil {
		t.Fatalf("failed response target = %+v, want nil", response.Target)
	}
	if response.GuestChannelCredentialHash != "" {
		t.Fatalf("failed response channel credential hash = %q, want empty", response.GuestChannelCredentialHash)
	}
	if got := testComputerMountPhaseError(response.Phases); !strings.Contains(got, "mount_path") {
		t.Fatalf("phase error = %q, want mount_path", got)
	}
	if err := <-errCh; err == nil || !strings.Contains(err.Error(), "mount_path") {
		t.Fatalf("handler error = %v, want mount_path", err)
	}
}

func testComputerMountPhaseError(phases []*computerv0.ComputerMountPhase) string {
	for i := len(phases) - 1; i >= 0; i-- {
		if phases[i] != nil && strings.TrimSpace(phases[i].GetError()) != "" {
			return strings.TrimSpace(phases[i].GetError())
		}
	}
	return ""
}

func computerMountPhaseNames(phases []*computerv0.ComputerMountPhase, expected ...string) bool {
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

func TestComputerOperationRegistryDefersRetiredCleanupUntilRelease(t *testing.T) {
	tempRoot := t.TempDir()
	oldRoot := filepath.Join(tempRoot, "old")
	if err := os.MkdirAll(oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	registry := newComputerOperationRegistry()
	registry.register("mat-1", &computerMountEntry{
		computerID:        "computer-1",
		channelCredential: "token-1",
		writerGeneration:  1,
		computerRoot:      filepath.Join(oldRoot, "computer"),
		cleanup:           func() { _ = os.RemoveAll(oldRoot) },
	})
	_, release, ok := registry.acquireExact("mat-1", "computer-1", "token-1", 1)
	if !ok {
		t.Fatal("expected registry acquire")
	}
	registry.register("mat-1", &computerMountEntry{
		computerID:        "computer-1",
		channelCredential: "token-2",
		writerGeneration:  2,
		computerRoot:      filepath.Join(tempRoot, "new", "computer"),
		cleanup:           func() {},
	})
	if _, err := os.Stat(oldRoot); err != nil {
		t.Fatalf("old computer root was cleaned while acquired: %v", err)
	}
	release()
	if _, err := os.Stat(oldRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old computer root after release err = %v, want not exist", err)
	}
}

func TestPreparedImageConfigRequiresMountedComputer(t *testing.T) {
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", "")
	_, cleanup, err := restorePreparedComputerImage(strings.NewReader(""), &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2, MountedImageConfig: &computerv0.RuntimeImageConfig{}})
	cleanup()
	if err == nil || !strings.Contains(err.Error(), "requires a mounted Computer") {
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
	registry := newComputerOperationRegistry()
	registry.setPreparedRuntime(&preparedComputerRuntime{computerID: "computer-1", writerGeneration: 2, computerInstanceID: "runtime", computerImageDigest: "image", computerMount: "/workspace", imageRoot: root, computerRoot: root, cleanup: func() {}})
	entry, err := restoreComputerMount(&computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerInstanceId: "runtime", ComputerId: "computer-1", WriterGeneration: 2}, MountPath: "/workspace", Target: testComputerMountTarget("version"), UsePreparedRuntime: true, ComputerImage: &computerv0.ComputerArtifact{Digest: "image", MediaType: computerImageMediaType, Encoding: computerImageEncoding, SizeBytes: 1}}, registry)
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
