//go:build linux

package firecracker

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestWritableDiskJailPreservesCaptureIdentity(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "disk.ext4")
	if err := os.WriteFile(source, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkWritableDiskIntoJail(source, dir, "disk.ext4", os.Getuid(), os.Getgid(), false); err != nil {
		t.Fatal(err)
	}
	a, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(a, b) {
		t.Fatal("writable disk was copied")
	}
	if err := os.WriteFile(target, []byte("vm write"), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(source)
	if err != nil || string(body) != "vm write" {
		t.Fatalf("capture sees %q: %v", body, err)
	}
	if err := linkWritableDiskIntoJail(source, dir, "disk.ext4", os.Getuid(), os.Getgid(), false); err != nil {
		t.Fatalf("existing exact link = %v", err)
	}
	occupied := filepath.Join(dir, "occupied")
	if err := os.WriteFile(occupied, []byte("another disk"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := linkWritableDiskIntoJail(source, dir, "occupied", os.Getuid(), os.Getgid(), false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("collision = %v", err)
	}
	body, err = os.ReadFile(occupied)
	if err != nil || string(body) != "another disk" {
		t.Fatal("existing disk overwritten")
	}
	link := filepath.Join(dir, "source-link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if err := linkWritableDiskIntoJail(link, dir, "bad", os.Getuid(), os.Getgid(), false); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestWritableDiskJailRejectsCrossFilesystemCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("disk"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/dev/shm", "helmr-disk-link-")
	if err != nil {
		t.Skipf("requires writable separate tmpfs: %v", err)
	}
	defer os.RemoveAll(root)
	var a, b syscall.Stat_t
	if err := syscall.Stat(source, &a); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Stat(root, &b); err != nil {
		t.Fatal(err)
	}
	if a.Dev == b.Dev {
		t.Skip("fixture filesystems coincide")
	}
	if err := linkWritableDiskIntoJail(source, root, "disk", os.Getuid(), os.Getgid(), false); !errors.Is(err, syscall.EXDEV) {
		t.Fatalf("cross-filesystem link = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "disk")); !os.IsNotExist(err) {
		t.Fatalf("copied disk left behind: %v", err)
	}
}
