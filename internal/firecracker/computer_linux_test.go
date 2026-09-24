//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/nbd"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestComputerAttachmentOwnsExactInode(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "working")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("customer state"), 512); err != nil {
		t.Fatal(err)
	}
	disk := &vm.RuntimeComputer{File: file, SizeBytes: 4096, VersionID: "01950000-0000-7000-8000-000000000001"}
	destination, err := attachComputerDisk(t.Context(), disk, dir, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attachComputerDisk(t.Context(), disk, dir, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("replaced existing disk")
	}
	original, _ := file.Stat()
	linked, err := os.Stat(destination)
	if err != nil || !os.SameFile(original, linked) {
		t.Fatalf("not same inode: %v", err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got[512:526]) != "customer state" {
		t.Fatalf("lost transferred disk: %v", err)
	}
	if err := validateComputerDisk(disk); err == nil {
		t.Fatal("closed source accepted")
	}
	drives := runtimeDrivesWithComputer("root", "scratch", "", destination, nil, nil)
	if len(drives) != 3 || *drives[2].DriveID != "computer" || *drives[2].IsReadOnly {
		t.Fatalf("computer is not writable third drive: %+v", drives)
	}
}

// Explicit opt-in: ordinary tests never claim a host block device.
func TestComputerBlockAttachmentAndPausedFlush(t *testing.T) {
	if os.Getenv("HELMR_DISPOSABLE_NBD_PROOF") != "1" {
		t.Skip("requires disposable NBD host")
	}
	helper := os.Getenv("HELMR_NBD_TEST_HELPER")
	if !filepath.IsAbs(helper) {
		t.Fatal("absolute NBD helper required")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	arena, err := os.MkdirTemp("", "fc-device-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retain on uncertainty:", arena)
	store, err := cas.NewFile(filepath.Join(arena, "base"))
	if err != nil {
		t.Fatal(err)
	}
	const key = "01992000-0000-7000-8000-000000000001"
	const size = 1 << 20
	keys := map[string][]byte{key: bytes.Repeat([]byte{7}, 32)}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: "fixture", ActiveKey: key, Keys: keys, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(ctx, size, 64)
	if err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, size)
	if err != nil {
		t.Fatal(err)
	}
	cfg := computer.LocalGenerationConfig{Directory: filepath.Join(arena, "local"), Base: root, BaseSource: store, Scope: "fixture", ActiveKey: key, Keys: keys, DirtyBlocks: 8, StagedBytes: 32 << 20, PackLimit: blockformat.MinPackLimit}
	generation, err := computer.CreateLocalGeneration(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	device, err := computer.AttachDevice(ctx, generation, nbd.Config{Helper: helper, Arena: arena, Socket: filepath.Join(arena, "nbd"), Size: size, Devices: []string{"/dev/nbd15", "/dev/nbd14"}})
	if err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	if err := device.BindConsumer(exited); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(arena, "instance")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	private, err := device.LinkInto(ctx, dir, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.OpenFile(private, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	disk := &vm.RuntimeComputer{File: source, SizeBytes: size, VersionID: key}
	disk.SizeBytes *= 2
	if err := validateComputerDisk(disk); err == nil {
		t.Fatal("block stat size used instead of actual capacity")
	}
	disk.SizeBytes = size
	attached, err := attachComputerDisk(ctx, disk, dir, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(arena, "vmm-jail")
	if err := os.Mkdir(jail, 0700); err != nil {
		t.Fatal(err)
	}
	jailed := filepath.Join(jail, filepath.Base(attached))
	if err := linkWritableDiskIntoJail(attached, jail, filepath.Base(attached), os.Getuid(), os.Getgid(), true); err != nil {
		t.Fatal(err)
	}
	if err := linkWritableDiskIntoJail(attached, jail, "not-computer", os.Getuid(), os.Getgid(), false); err == nil {
		t.Fatal("block device accepted as scratch or snapshot memory")
	}
	scratch := filepath.Join(dir, "scratch.ext4")
	if err := os.WriteFile(scratch, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := openRuntimeDiskFiles(scratch, attached)
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xad}, 4096)
	if _, err := files["computer"].WriteAt(data, 4096); err != nil {
		t.Fatal(err)
	}
	if err := syncPausedBacking(files["computer"], attached, jailed); err != nil {
		t.Fatal(err)
	}
	// A replacement pathname is not proof of the retained device identity.
	replacement := filepath.Join(jail, "replacement")
	if err := os.WriteFile(replacement, make([]byte, 4096), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncPausedBacking(files["computer"], attached, replacement); err == nil {
		t.Fatal("replacement inode accepted")
	}
	if err := closeRuntimeDiskFiles(files); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	close(exited)
	if err := device.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened, err := computer.OpenLocalGeneration(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(data))
	if _, err := reopened.ReadAt(ctx, got, 4096); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("paused barrier did not persist generation: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(arena); err != nil {
		t.Fatal(err)
	}
}
