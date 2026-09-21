//go:build linux

package firecracker

import (
	"os"
	"path/filepath"
	"testing"

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
