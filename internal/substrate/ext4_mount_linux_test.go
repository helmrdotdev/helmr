//go:build linux

package substrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// This opt-in proof requires a disposable local Linux mount namespace. Production
// preparation never mounts the customer filesystem in the host kernel.
func proveNonrootImageWrite(t *testing.T, image string) {
	t.Helper()
	if os.Getenv("HELMR_SUBSTRATE_MOUNT_PROOF") != "1" {
		return
	}
	if os.Geteuid() != 0 {
		t.Fatal("mount proof requires root")
	}
	mountpoint, err := os.MkdirTemp("", "helmr-image-mount-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mountpoint, 0755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("mount", "-o", "loop,nosuid,nodev", image, mountpoint).CombinedOutput(); err != nil {
		_ = os.Remove(mountpoint)
		t.Fatalf("mount proof: %v: %s", err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mountpoint).CombinedOutput(); err != nil {
			t.Errorf("unmount proof: %v: %s (retained %s)", err, out, mountpoint)
			return
		}
		if err := os.Remove(mountpoint); err != nil {
			t.Error(err)
		}
	}()
	command := exec.Command("/bin/sh", "-c", "umask 077; printf success > created-by-agent")
	command.Dir = filepath.Join(mountpoint, "home", "agent")
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000}}
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("nonroot write: %v: %s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(command.Dir, "created-by-agent"))
	if err != nil || string(body) != "success" {
		t.Fatalf("nonroot content=%q err=%v", body, err)
	}
}
