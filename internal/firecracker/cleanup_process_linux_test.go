//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

func TestCleanupProcessChild(t *testing.T) {
	if os.Getenv("HELMR_CLEANUP_CHILD") != "1" {
		return
	}
	if root := os.Getenv("HELMR_CLEANUP_ROOT"); root != "" {
		if err := unix.Chroot(root); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := os.Chdir("/"); err != nil {
			os.Exit(2)
		}
	}
	if os.Getenv("HELMR_CLEANUP_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	ready := os.NewFile(3, "ready")
	if _, err := ready.Write([]byte("ready")); err != nil {
		os.Exit(2)
	}
	ready.Close()
	for {
		time.Sleep(time.Hour)
	}
}

func startCleanupProcess(t *testing.T, root, id string, ignoreTerm bool) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cmd := exec.Command(executable, "-test.run=^TestCleanupProcessChild$", "--")
	cmd.Args[0] = "/firecracker"
	if id != "" {
		cmd.Args = append(cmd.Args, "--id", id)
	}
	cmd.Env = append(os.Environ(), "HELMR_CLEANUP_CHILD=1", "HELMR_CLEANUP_ROOT="+root)
	if ignoreTerm {
		cmd.Env = append(cmd.Env, "HELMR_CLEANUP_IGNORE_TERM=1")
	}
	cmd.ExtraFiles = []*os.File{w}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		w.Close()
		t.Fatal(err)
	}
	w.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ready, err := io.ReadAll(r)
	if err != nil || string(ready) != "ready" {
		t.Fatalf("readiness %q: %v", ready, err)
	}
	return cmd
}

func cleanupProcessFixture(t *testing.T) (*Connector, vm.Owner, string, *ownedComputerFixture) {
	t.Helper()
	owner := vm.Owner{Kind: vm.OwnerInstance, ID: "019fc619-8443-77f6-9498-8c348c25f708"}
	c := &Connector{cfg: Config{StateDir: t.TempDir(), JailerChrootBaseDir: t.TempDir(), IPPath: "/bin/true"}, computerDevices: &sync.Map{}}
	if _, err := createOwnerStateRoot(c.cfg.StateDir, owner); err != nil {
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
	return c, owner, jail, device
}

func assertCleanupProcessRetained(t *testing.T, c *Connector, owner vm.Owner, jail string, device *ownedComputerFixture, pid int) {
	t.Helper()
	if err := validateOwnerMarker(filepath.Join(c.cfg.StateDir, owner.ID), owner); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jail); err != nil {
		t.Fatal("jail lost", err)
	}
	if device.closes != 0 {
		t.Fatal("device released")
	}
	select {
	case <-device.excluded:
		t.Fatal("consumer exclusion asserted")
	default:
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if _, err := unix.Poll(polls, 0); err != nil || polls[0].Revents != 0 {
		t.Fatalf("unowned/cancelled process stopped: %v %v", polls, err)
	}
}

func TestCleanupRejectsCommandIdentityWithoutOwnedRoot(t *testing.T) {
	c, owner, jail, device := cleanupProcessFixture(t)
	child := startCleanupProcess(t, "", owner.ID, false)
	var unproven *vm.CleanupUnprovenError
	if err := c.cleanup(t.Context(), owner); !errors.As(err, &unproven) {
		t.Fatalf("foreign root accepted: %v", err)
	}
	assertCleanupProcessRetained(t, c, owner, jail, device, child.Process.Pid)
}

func TestCleanupOwnedProcessKernel(t *testing.T) {
	if os.Getenv("HELMR_RECOVERY_KERNEL_TEST") != "1" {
		t.Skip("requires explicitly isolated privileged Linux test host")
	}
	for _, ignoreTerm := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore-term-%t", ignoreTerm), func(t *testing.T) {
			c, owner, jail, device := cleanupProcessFixture(t)
			child := startCleanupProcess(t, jail, "", ignoreTerm)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := c.cleanup(ctx, owner); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			assertCleanupProcessRetained(t, c, owner, jail, device, child.Process.Pid)
			fd, err := unix.PidfdOpen(child.Process.Pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if err := c.cleanup(t.Context(), owner); err != nil {
				t.Fatal(err)
			}
			polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(polls, 0); err != nil || polls[0].Revents&unix.POLLIN == 0 {
				t.Fatalf("cleanup returned before exit: %v %v", polls, err)
			}
			if device.closes != 1 {
				t.Fatalf("device closes=%d", device.closes)
			}
			for _, path := range []string{jail, filepath.Join(c.cfg.StateDir, owner.ID)} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("retained %s: %v", path, err)
				}
			}
			if err := c.cleanup(t.Context(), owner); err != nil {
				t.Fatal("absent retry", err)
			}
			if device.closes != 1 {
				t.Fatal("absent retry released device twice")
			}
		})
	}
}
