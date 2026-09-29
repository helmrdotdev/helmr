package guestd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
)

func TestComputerPreparationUsesMountedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", root)
	marker := filepath.Join(root, "customer-state")
	if err := os.WriteFile(marker, []byte("retained"), 0600); err != nil {
		t.Fatal(err)
	}
	request := &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2, MountedImageConfig: &computerv0.RuntimeImageConfig{WorkingDir: "/project", User: "1000:1000"}}
	image, cleanup, err := restorePreparedComputerImage(strings.NewReader("not an image stream"), request)
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
	if _, _, err := restorePreparedComputerImage(strings.NewReader(""), request); err == nil || !strings.Contains(err.Error(), "admitted image config") {
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
	artifact := &computerv0.ComputerArtifact{
		Digest: "sha256:seed", MediaType: computer.SeedMediaType,
		Encoding: "oci-tar", SizeBytes: 79_664_879,
	}
	prepared, _, err := restorePreparedComputerRuntime(strings.NewReader("no image stream"), &computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2,
		ComputerInstanceId: "runtime", MountPath: "/workspace", ComputerImage: artifact,
		MountedImageConfig: &computerv0.RuntimeImageConfig{WorkingDir: "/workspace", User: "0:0"},
	}, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	registry := newComputerOperationRegistry()
	registry.setPreparedRuntime(prepared)
	request := &computerv0.MaterializeComputerRequest{
		Envelope:  &computerv0.ComputerOperationEnvelope{ComputerInstanceId: "runtime", ComputerId: "computer-1", WriterGeneration: 2},
		MountPath: "/workspace", Target: testComputerMountTarget("version"),
		UsePreparedRuntime: true, ComputerImage: artifact,
	}
	for name, change := range map[string]func(*computerv0.MaterializeComputerRequest){
		"writer":  func(r *computerv0.MaterializeComputerRequest) { r.Envelope.WriterGeneration++ },
		"runtime": func(r *computerv0.MaterializeComputerRequest) { r.Envelope.ComputerInstanceId = "other" },
		"digest":  func(r *computerv0.MaterializeComputerRequest) { r.ComputerImage.Digest = "sha256:other" },
		"mount":   func(r *computerv0.MaterializeComputerRequest) { r.MountPath = "/other" },
	} {
		t.Run(name, func(t *testing.T) {
			mismatch := proto.Clone(request).(*computerv0.MaterializeComputerRequest)
			change(mismatch)
			if _, err := restoreComputerMount(mismatch, registry); err == nil || !strings.Contains(err.Error(), "prepared computer runtime is not available") {
				t.Fatalf("mismatched prepared identity: %v", err)
			}
		})
	}
	entry, err := restoreComputerMount(request, registry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.imageRoot != root || entry.computerRoot != filepath.Join(root, "workspace") {
		t.Fatalf("materialization changed mounted root: %+v", entry)
	}
	if _, err := restoreComputerMount(request, registry); err == nil || !strings.Contains(err.Error(), "prepared computer runtime is not available") {
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
