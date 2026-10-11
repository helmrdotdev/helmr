//go:build linux

package firecracker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/vm"
)

func TestJailerCgroupPath(t *testing.T) {
	mounts := "tmpfs /sys/fs/cgroup tmpfs rw 0 0\n" +
		"cgroup /sys/fs/cgroup/cpu cgroup rw,cpu 0 0\n" +
		"cgroup /sys/fs/cgroup/cpuset cgroup rw,cpuset 0 0\n" +
		"cgroup2 /sys/fs/cgroup/unified cgroup2 rw 0 0\n" +
		"cgroup2 /another cgroup2 rw 0 0\n"
	for _, tt := range []struct{ version, want string }{
		{"1", "/sys/fs/cgroup/cpuset/firecracker/owner"},
		{"2", "/sys/fs/cgroup/unified/firecracker/owner"},
		{"", "/sys/fs/cgroup/unified/firecracker/owner"},
	} {
		got, err := jailerCgroupPath(mounts, tt.version, "owner")
		if err != nil || got != tt.want {
			t.Fatalf("version %q: %q, %v", tt.version, got, err)
		}
	}
	if _, err := jailerCgroupPath(mounts, "3", "owner"); err == nil {
		t.Fatal("accepted unknown version")
	}
	if _, err := jailerCgroupPath("", "2", "owner"); err == nil {
		t.Fatal("accepted missing mount")
	}

}

// This exercises the kernel's populated/child-group exclusion and the complete
// cleanup operation. Run only in a disposable writable cgroup namespace.
func TestCleanupExactCgroup(t *testing.T) {
	if os.Getenv("HELMR_PRIVILEGED_CGROUP_TEST") != "1" {
		t.Skip("requires disposable writable cgroup namespace")
	}
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019fc619-8443-77f6-9498-8c348c25f709"}
	path, err := runtimeCgroupPath("2", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(path)
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(parent); err != nil {
			t.Error(err)
		}
	})
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	neighbor := filepath.Join(parent, "unrelated")
	if err := os.Mkdir(neighbor, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(neighbor); err != nil {
			t.Error(err)
		}
	})
	c := &Connector{cfg: Config{StateDir: t.TempDir(), JailerChrootBaseDir: t.TempDir(), IPPath: "/bin/true", CgroupVersion: "2"}, computerDevices: &sync.Map{}}
	var unproven *vm.CleanupUnprovenError
	if err := c.cleanup(context.Background(), owner); !errors.As(err, &unproven) {
		t.Fatalf("missing marker: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("removed unowned cgroup", err)
	}
	state := filepath.Join(c.cfg.StateDir, owner.ID)
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "owner"), []byte(string(owner.Kind)+"\n"+owner.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(c.cfg.JailerChrootBaseDir, "firecracker", owner.ID, "root")
	if err := os.MkdirAll(jail, 0700); err != nil {
		t.Fatal(err)
	}
	device := &ownedComputerFixture{}
	retained := c.lockComputerOwner(owner)
	if err := retainComputerDevice(retained, device); err != nil {
		t.Fatal(err)
	}
	retained.mu.Unlock()
	assertRetained := func() {
		t.Helper()
		if _, err := os.Stat(jail); err != nil {
			t.Fatal("lost jail before cgroup absence", err)
		}
		if device.closes != 0 {
			t.Fatal("released device before cgroup absence")
		}
		select {
		case <-device.excluded:
			t.Fatal("excluded live cgroup")
		default:
		}
	}
	child := filepath.Join(path, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := c.cleanup(context.Background(), owner); !errors.As(err, &unproven) {
		t.Fatalf("child group: %v", err)
	}
	if err := validateOwnerMarker(state, owner); err != nil {
		t.Fatal("lost retry authority", err)
	}
	assertRetained()
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.cleanup(context.Background(), owner); !errors.As(err, &unproven) {
		t.Fatalf("populated group: %v", err)
	}
	if err := validateOwnerMarker(state, owner); err != nil {
		t.Fatal("lost retry authority", err)
	}
	assertRetained()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if err := c.cleanup(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	if err := c.cleanup(context.Background(), owner); err != nil {
		t.Fatal("repeat cleanup", err)
	}
	if device.closes != 1 {
		t.Fatalf("device closes=%d", device.closes)
	}
	for _, gone := range []string{path, state, jail} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("not absent %s: %v", gone, err)
		}
	}
	if _, err := os.Stat(neighbor); err != nil {
		t.Fatal("unrelated cgroup changed", err)
	}
}
