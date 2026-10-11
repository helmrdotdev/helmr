//go:build linux

package custody

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sys/unix"
)

func TestRecoveryPrivateRootChild(t *testing.T) {
	root := os.Getenv("HELMR_RECOVERY_CHILD_ROOT")
	if root == "" {
		return
	}
	runtime.LockOSThread()
	must := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	must(unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""))
	must(unix.Mount(root, root, "", unix.MS_BIND, ""))
	old := filepath.Join(root, "old")
	must(os.Mkdir(old, 0700))
	must(unix.PivotRoot(root, old))
	must(os.Chdir("/"))
	must(unix.Unmount("/old", unix.MNT_DETACH))
	must(os.Remove("/old"))
	if os.Getenv("HELMR_RECOVERY_IGNORE_TERM") == "1" {
		signal.Ignore(syscall.SIGTERM)
	}
	ready := os.NewFile(3, "ready")
	_, err := ready.Write([]byte("ready"))
	must(err)
	must(ready.Close())
	for {
		time.Sleep(time.Hour)
	}
}

func startRecoveryPrivateChild(t *testing.T, root string, ignoreTerm bool) *exec.Cmd {
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
	cmd := exec.Command(executable, "-test.run=^TestRecoveryPrivateRootChild$")
	cmd.Args[0] = "/firecracker"
	cmd.Env = append(os.Environ(), "HELMR_RECOVERY_CHILD_ROOT="+root)
	if ignoreTerm {
		cmd.Env = append(cmd.Env, "HELMR_RECOVERY_IGNORE_TERM=1")
	}
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
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
		t.Fatalf("child readiness %q: %v", ready, err)
	}
	return cmd
}

func TestRecoveryPrivateRootKernel(t *testing.T) {
	if os.Getenv("HELMR_RECOVERY_KERNEL_TEST") != "1" {
		t.Skip("requires an explicitly isolated privileged Linux test host")
	}
	const id = "019c10d5-a6f7-7af1-8f5f-000000000107"
	for _, ignoreTerm := range []bool{false, true} {
		t.Run(fmt.Sprintf("ignore-term-%t", ignoreTerm), func(t *testing.T) {
			work, jailer := t.TempDir(), t.TempDir()
			root := makeRecoveryOwner(t, work, jailer, id)
			child := startRecoveryPrivateChild(t, root, ignoreTerm)
			pid := child.Process.Pid
			rendered, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "root"))
			if err != nil || rendered != "/" {
				t.Fatalf("private root rendering=%q: %v", rendered, err)
			}
			inventory, err := Processes(StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, p := range inventory {
				found = found || p.PID == pid && p.ID == id && p.Problem == ""
			}
			if !found {
				t.Fatalf("private-root process not recognized: %+v", inventory)
			}
			owner := vm.Owner{Kind: vm.OwnerInstance, ID: id}
			otherID := "019c10d5-a6f7-7af1-8f5f-000000000108"
			makeRecoveryOwner(t, work, jailer, otherID)
			if err := Stop(t.Context(), StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer, vm.Owner{Kind: vm.OwnerInstance, ID: otherID}, pid); err == nil {
				t.Fatal("wrong owner authorized signal")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := Stop(ctx, StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer, owner, pid); err != context.Canceled {
				t.Fatalf("cancelled stop=%v", err)
			}

			marker := filepath.Join(work, "vms", "guest", id, "owner")
			original, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := Stop(t.Context(), StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer, owner, pid); err == nil {
				t.Fatal("changed marker authorized signal")
			}
			if err := os.WriteFile(marker, original, 0600); err != nil {
				t.Fatal(err)
			}

			movedRoot := root + ".retained"
			if err := os.Rename(root, movedRoot); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(root, 0700); err != nil {
				t.Fatal(err)
			}
			if err := Stop(t.Context(), StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer, owner, pid); err == nil {
				t.Fatal("replaced jail root authorized signal")
			}
			if err := os.Remove(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(movedRoot, root); err != nil {
				t.Fatal(err)
			}
			handle, err := unix.PidfdOpen(pid, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(handle)
			if exited, err := recoveryProcessExited(handle); err != nil || exited {
				t.Fatalf("negative checks stopped child: %v %v", exited, err)
			}
			if err := Stop(t.Context(), StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer, owner, pid); err != nil {
				t.Fatal(err)
			}
			if exited, err := recoveryProcessExited(handle); err != nil || !exited {
				t.Fatalf("stop returned without exit: %v %v", exited, err)
			}
		})
	}
}

func TestRecoveryDuplicateJailIdentityKernel(t *testing.T) {
	if os.Getenv("HELMR_RECOVERY_KERNEL_TEST") != "1" {
		t.Skip("requires an explicitly isolated privileged Linux test host")
	}
	work, jailer := t.TempDir(), t.TempDir()
	first := makeRecoveryOwner(t, work, jailer, "019c10d5-a6f7-7af1-8f5f-000000000107")
	second := makeRecoveryOwner(t, work, jailer, "019c10d5-a6f7-7af1-8f5f-000000000108")
	if err := unix.Mount(first, second, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(second, 0); err != nil {
			t.Errorf("unmount fixture: %v", err)
		}
	})
	roots, err := openRecoveryRoots(StateRoot{Base: work, Components: []string{"vms", "guest"}}, jailer)
	defer closeRecoveryRoots(roots)
	if err == nil {
		t.Fatal("duplicate jail identity granted recovery authority")
	}
}
