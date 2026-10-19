//go:build linux && computerproof

package guestd

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPreparationFinalizesCleanExt4(t *testing.T) {
	if os.Getenv("HELMR_PREPARATION_EXT4_HELPER") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPreparationFinalizesCleanExt4$", "-test.v")
		cmd.Env = append(os.Environ(), "HELMR_PREPARATION_EXT4_HELPER=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("ext4 preparation: %v\n%s", err, out)
		}
		t.Log(string(out))
		return
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	disk := filepath.Join(dir, "source.ext4")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	ext4ProofCommand(t, "mke2fs", "-q", "-F", "-t", "ext4", disk)
	root, device := computerLoopMount(t, disk)
	control, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	handle, err := openComputerFilesystemHandle(root, device, control)
	if err != nil {
		t.Fatal(err)
	}
	w := &computerWriteback{
		syncFilesystem:   func() error { return unix.Syncfs(int(handle.Fd())) },
		freezeFilesystem: func() error { return unix.IoctlSetInt(int(handle.Fd()), computerFreezeIOCTL, 0) },
		closeFilesystem:  handle.Close,
	}
	// Only this disposable test mount is thawed for unmount cleanup. Production
	// destroys its private VM while still frozen and never resumes source writes.
	t.Cleanup(func() {
		if w.frozen {
			if err := unix.IoctlSetInt(int(handle.Fd()), computerThawIOCTL, 0); err != nil {
				t.Error(err)
			}
		}
		if err := w.close(); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(filepath.Join(root, "prepared"), []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := w.finalizePreparation(t.Context()); err != nil {
		t.Fatal(err)
	}
	var features [4]byte
	if _, err := f.ReadAt(features[:], 1024+96); err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint32(features[:])&4 != 0 {
		t.Fatal("frozen source still needs ext4 journal recovery")
	}
	clone := filepath.Join(dir, "clone.ext4")
	out, err := os.Create(clone)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(out, f)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		t.Fatalf("copy: %v %v", copyErr, closeErr)
	}
	restored, _ := computerLoopMount(t, clone)
	body, err := os.ReadFile(filepath.Join(restored, "prepared"))
	if err != nil || string(body) != "complete" {
		t.Fatalf("restored: %q %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(restored, "independent"), []byte("writable"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Log("terminal preparation clears journal recovery and produces independent writable clone")
}
