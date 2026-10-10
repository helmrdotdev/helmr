package guestd

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/wire"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	image, cleanup, err := restorePreparedComputerImage(request)
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
	if _, _, err := restorePreparedComputerImage(request); err == nil || !strings.Contains(err.Error(), "admitted image config") {
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
	prepared, _, err := restorePreparedComputerRuntime(&computerv0.PrepareComputerRuntimeRequest{ComputerId: "computer-1", WriterGeneration: 2,
		ComputerInstanceId: "instance", MountPath: "/workspace",
		MountedImageConfig: &computerv0.RuntimeImageConfig{WorkingDir: "/workspace", User: "0:0"},
	}, slogDiscard())
	if err != nil {
		t.Fatal(err)
	}
	registry := newComputerOperationRegistry()
	registry.setPreparedRuntime(prepared)
	request := &computerv0.MaterializeComputerRequest{
		Envelope:  &computerv0.ComputerOperationEnvelope{ComputerInstanceId: "instance", ComputerId: "computer-1", WriterGeneration: 2},
		MountPath: "/workspace", Target: testComputerMountTarget("version"),
	}
	for name, change := range map[string]func(*computerv0.MaterializeComputerRequest){
		"writer":   func(r *computerv0.MaterializeComputerRequest) { r.Envelope.WriterGeneration++ },
		"instance": func(r *computerv0.MaterializeComputerRequest) { r.Envelope.ComputerInstanceId = "other" },
		"mount":    func(r *computerv0.MaterializeComputerRequest) { r.MountPath = "/other" },
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

// Both allocation kinds send the mounted-image protocol. It must complete without
// an OCI artifact descriptor or a second body stream after either request.
func TestAllocatedMountedComputerPreparationAndMaterialization(t *testing.T) {
	t.Setenv("HELMR_GUESTD_COMPUTER_ROOT", t.TempDir())
	registry := newComputerOperationRegistry()
	machine := &agentHostTestMachine{registry: registry}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	exchange := func(kind wire.StreamType, request, response proto.Message) {
		t.Helper()
		stream, err := machine.OpenStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
		defer stop()
		if err := wire.WriteStreamFrameHeader(stream, wire.StreamHeader{Type: kind}, 0); err != nil {
			t.Fatal(err)
		}
		if err := frameio.WriteProtoFrame(stream, request); err != nil {
			t.Fatal(err)
		}
		if err := frameio.ReadProtoFrame(stream, response); err != nil {
			t.Fatal(err)
		}
	}
	var prepared computerv0.PrepareComputerRuntimeResponse
	exchange(wire.StreamTypeComputerRuntimePrepare, &computerv0.PrepareComputerRuntimeRequest{
		ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 3,
		MountPath: "/workspace", MountedImageConfig: &computerv0.RuntimeImageConfig{User: "0:0", WorkingDir: "/workspace"},
	}, &prepared)
	if prepared.Status != "prepared" {
		t.Fatalf("preparation: %v", &prepared)
	}
	var mounted computerv0.MaterializeComputerResponse
	exchange(wire.StreamTypeComputerMaterialize, &computerv0.MaterializeComputerRequest{
		Envelope:  &computerv0.ComputerOperationEnvelope{ComputerId: "computer", ComputerInstanceId: "instance", WriterGeneration: 3, ChannelCredential: "channel"},
		MountPath: "/workspace", Target: &computerv0.ComputerMountTarget{BaseComputerDiskVersionId: "version"},
	}, &mounted)
	if mounted.Status != "running" || mounted.GetTarget().GetBaseComputerDiskVersionId() != "version" || mounted.GuestChannelCredentialHash != sha256sum.HexBytes([]byte("channel")) {
		t.Fatalf("materialization: %v", &mounted)
	}
}
