package guestd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/frameio"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
)

func testComputerMountTarget(versionID string) *computerv0.ComputerMountTarget {
	return &computerv0.ComputerMountTarget{
		BaseComputerDiskVersionId: versionID,
	}
}

func TestComputerRuntimePrepareUsesMountedImageAndComputerInstanceID(t *testing.T) {
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", t.TempDir())
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
		MountedImageConfig: &computerv0.RuntimeImageConfig{},
	}); err != nil {
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
	if _, ok := registry.takePreparedRuntime("computer-instance-other", "computer-1", "/workspace", 2); ok {
		t.Fatal("prepared runtime accepted a different computer_instance_id")
	}
	if _, ok := registry.takePreparedRuntime(strings.TrimSpace(computerInstanceID), "computer-1", "/workspace", 2); ok {
		t.Fatal("prepared runtime normalized an opaque computer_instance_id")
	}
	prepared, ok := registry.takePreparedRuntime(computerInstanceID, "computer-1", "/workspace", 2)
	if !ok {
		t.Fatal("prepared runtime did not accept the matching computer_instance_id")
	}
	prepared.cleanup()
}

func TestComputerRuntimePreparationRequiresIdentity(t *testing.T) {
	_, _, err := restorePreparedComputerRuntime(&computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2, ComputerInstanceId: "   ", MountPath: "/workspace"}, slogDiscard())
	if err == nil || !strings.Contains(err.Error(), "identity is required") {
		t.Fatalf("invalid identity: %v", err)
	}
}

func TestComputerMaterializeReturnsFailureResponse(t *testing.T) {
	registry := newComputerOperationRegistry()
	materializeClient, materializeServer := net.Pipe()
	defer materializeClient.Close()
	defer materializeServer.Close()
	errCh := make(chan error, 1)
	go func() {
		errCh <- handleComputerMaterializeConnection(context.Background(), materializeServer, slogDiscard(), registry)
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
	_, cleanup, err := restorePreparedComputerImage(&computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2, MountedImageConfig: &computerv0.RuntimeImageConfig{}})
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
	registry.setPreparedRuntime(&preparedComputerRuntime{computerID: "computer-1", writerGeneration: 2, computerInstanceID: "instance", computerMount: "/workspace", imageRoot: root, computerRoot: root, cleanup: func() {}})
	entry, err := restoreComputerMount(&computerv0.MaterializeComputerRequest{Envelope: &computerv0.ComputerOperationEnvelope{ComputerInstanceId: "instance", ComputerId: "computer-1", WriterGeneration: 2}, MountPath: "/workspace", Target: testComputerMountTarget("version")}, registry)
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
