package guestd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	workspacev0 "github.com/helmrdotdev/helmr/internal/proto/workspace/v0"
	"google.golang.org/protobuf/proto"
)

func TestComputerPreparationUsesMountedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", root)
	marker := filepath.Join(root, "customer-state")
	if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &workspacev0.PrepareWorkspaceRuntimeRequest{MountedImageConfig: &workspacev0.RuntimeImageConfig{WorkingDir: "/project", User: "1000:1000"}}
	image, cleanup, err := restorePreparedWorkspaceImage(strings.NewReader("not an image stream"), request)
	if err != nil {
		t.Fatal(err)
	}
	if image.RootfsDir != root || image.Config.WorkingDir != "/project" || image.Config.User != "1000:1000" {
		t.Fatalf("wrong mounted environment: %+v", image)
	}
	cleanup()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cleanup removed customer disk: %v", err)
	}
	request.MountedImageConfig = nil
	if _, _, err := restorePreparedWorkspaceImage(strings.NewReader(""), request); err == nil || !strings.Contains(err.Error(), "admitted image config") {
		t.Fatalf("missing config tried stream fallback: %v", err)
	}
}

func TestPreparedComputerMaterializationUsesMountedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", root)
	marker := filepath.Join(root, "customer-state")
	if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("customer-state", filepath.Join(root, "customer-link")); err != nil {
		t.Fatal(err)
	}
	artifact := &workspacev0.WorkspaceArtifact{
		Digest: "sha256:seed", MediaType: computer.SeedMediaType,
		Encoding: "oci-tar", SizeBytes: 79_664_879,
	}
	prepared, _, err := restorePreparedWorkspaceRuntime(strings.NewReader("no image stream"), &workspacev0.PrepareWorkspaceRuntimeRequest{
		RuntimeInstanceId: "runtime", MountPath: "/workspace", WorkspaceImage: artifact,
		MountedImageConfig: &workspacev0.RuntimeImageConfig{WorkingDir: "/workspace", User: "0:0"},
	}, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	registry := newWorkspaceOperationRegistry()
	registry.setPreparedRuntime(prepared)
	request := &workspacev0.MaterializeWorkspaceRequest{
		Envelope:  &workspacev0.WorkspaceOperationEnvelope{WorkspaceMountId: "mount"},
		MountPath: "/workspace", Target: testComputerMountTarget("version"),
		RuntimeInstanceId: "runtime", UsePreparedRuntime: true, WorkspaceImage: artifact,
	}
	for name, change := range map[string]func(*workspacev0.MaterializeWorkspaceRequest){
		"runtime": func(r *workspacev0.MaterializeWorkspaceRequest) { r.RuntimeInstanceId = "other" },
		"digest":  func(r *workspacev0.MaterializeWorkspaceRequest) { r.WorkspaceImage.Digest = "sha256:other" },
		"mount":   func(r *workspacev0.MaterializeWorkspaceRequest) { r.MountPath = "/other" },
	} {
		t.Run(name, func(t *testing.T) {
			mismatch := proto.Clone(request).(*workspacev0.MaterializeWorkspaceRequest)
			change(mismatch)
			if _, err := restoreWorkspaceMount(mismatch, registry); err == nil || !strings.Contains(err.Error(), "prepared computer runtime is not available") {
				t.Fatalf("mismatched prepared identity: %v", err)
			}
		})
	}
	entry, err := restoreWorkspaceMount(request, registry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.imageRoot != root || entry.workspaceRoot != filepath.Join(root, "workspace") {
		t.Fatalf("materialization changed mounted root: %+v", entry)
	}
	if _, err := restoreWorkspaceMount(request, registry); err == nil || !strings.Contains(err.Error(), "prepared computer runtime is not available") {
		t.Fatalf("prepared runtime reused: %v", err)
	}
	entry.cleanup()
	if got, err := os.ReadFile(marker); err != nil || string(got) != "retained" {
		t.Fatalf("mounted file changed: %q, %v", got, err)
	}
	if got, err := os.Readlink(filepath.Join(root, "customer-link")); err != nil || got != "customer-state" {
		t.Fatalf("mounted symlink changed: %q, %v", got, err)
	}
}
