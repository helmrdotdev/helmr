//go:build linux && computerproof

package guestd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Requires only newly allocated loop devices backed by private disposable files.
// It must not be run with a host root or existing disk as the customer mount.
func TestComputerExt4CaptureProof(t *testing.T) {
	if os.Getenv("HELMR_EXT4_CAPTURE_HELPER") != "1" {
		command := exec.Command(os.Args[0], "-test.run=^TestComputerExt4CaptureProof$", "-test.v")
		command.Env = append(os.Environ(), "HELMR_EXT4_CAPTURE_HELPER=1")
		command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("ext4 capture: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	selfNS, err := os.Stat("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	parentNS, err := os.Stat(fmt.Sprintf("/proc/%d/ns/mnt", os.Getppid()))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(selfNS, parentNS) {
		t.Fatal("helper must have a private mount namespace")
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	disk := filepath.Join(dir, "computer.ext4")
	file, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(128 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ext4ProofCommand(t, "mke2fs", "-q", "-F", "-t", "ext4", disk)
	root, device := computerLoopMount(t, disk)
	control, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	filesystem, err := openComputerFilesystem(root, device, control)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := filesystem.close(); err != nil {
			t.Error(err)
		}
	})
	for _, invalid := range []string{dir, filepath.Join(root, "lost+found")} {
		if handle, err := openComputerFilesystem(invalid, device, control); err == nil {
			_ = handle.close()
			t.Fatalf("accepted non-mount %s", invalid)
		}
	}
	link := filepath.Join(dir, "mount-link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if handle, err := openComputerFilesystem(link, device, control); err == nil {
		_ = handle.close()
		t.Fatal("accepted symlink mount")
	}
	sameControl, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer sameControl.Close()
	if handle, err := openComputerFilesystem(root, device, sameControl); err == nil {
		_ = handle.close()
		t.Fatal("accepted customer disk as control filesystem")
	}
	wrongDevice, err := os.Open("/dev/null")
	if err != nil {
		t.Fatal(err)
	}
	defer wrongDevice.Close()
	if handle, err := openComputerFilesystem(root, wrongDevice, control); err == nil {
		_ = handle.close()
		t.Fatal("accepted non-block device")
	}
	view := filepath.Join(dir, "view")
	program, runtime, grants := filepath.Join(dir, "program"), filepath.Join(dir, "runtime"), filepath.Join(dir, "grants")
	for _, p := range []string{view, program, runtime, grants} {
		if err := os.Mkdir(p, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(program, "tool"), []byte("pinned tool"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(grants, "authority"), []byte("live grant"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanup, err := mountComputerRoot(root, view, program, runtime, grants)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Error(err)
		}
	})
	if err := os.MkdirAll(filepath.Join(view, "home/agent"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/helmr/program/tool", filepath.Join(view, "home/agent/tool")); err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(view, "home/agent/history")
	dirty, err := os.OpenFile(payload, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dirty.Close() })
	if err := dirty.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	memory, err := unix.Mmap(int(dirty.Fd()), 0, 4096, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if memory != nil {
			if err := unix.Munmap(memory); err != nil {
				t.Error(err)
			}
		}
	})
	copy(memory, []byte("before capture")) // Deliberately no msync or file.Sync.
	cgroupRoot, err := os.MkdirTemp("/sys/fs/cgroup", "helmr-ext4-proof-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(cgroupRoot); err != nil {
			t.Error(err)
		}
	})
	tree := computerProofScope(t, cgroupRoot, "tree")
	parent := computerProofScope(t, tree.path, "parent")
	childTree := computerProofScope(t, tree.path, "child")
	child := computerProofScope(t, childTree.path, "process")
	parentFile := filepath.Join(view, "home/agent/parent")
	childFile := filepath.Join(view, "home/agent/child")
	paths := []string{parentFile, parentFile + ".descendant", childFile, childFile + ".descendant"}
	computerProofWriter(t, parent, parentFile)
	computerProofWriter(t, child, childFile)
	computerProofGrowth(t, paths)
	freezeCtx, cancelFreeze := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelFreeze()
	if err := tree.freeze(freezeCtx); err != nil {
		t.Fatal(err)
	}
	frozenWriters := computerProofContents(t, paths)
	if err := filesystem.freeze(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := filesystem.thaw(); err != nil {
			t.Error(err)
		}
	})
	// The same handle rejects re-freeze without releasing its freeze.
	if err := filesystem.freeze(); err == nil {
		t.Fatal("duplicate filesystem freeze accepted")
	}
	saved := filepath.Join(dir, "saved.ext4")
	in, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(saved, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.thaw(); err != nil {
		t.Fatal(err)
	}
	thawCtx, cancelThaw := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelThaw()
	if err := tree.thaw(thawCtx); err != nil {
		t.Fatal(err)
	}
	computerProofGrowth(t, paths)
	if err := tree.kill(); err != nil {
		t.Fatal(err)
	}
	if err := tree.waitEmpty(); err != nil {
		t.Fatal(err)
	}
	copy(memory, []byte("after capture!"))
	if err := unix.Munmap(memory); err != nil {
		t.Fatal(err)
	}
	memory = nil
	if err := dirty.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err != nil {
		t.Fatal(err)
	}
	ext4ProofCommand(t, "e2fsck", "-fn", saved)
	restored, restoredDevice := computerLoopMount(t, saved)
	if handle, err := openComputerFilesystem(root, restoredDevice, control); err == nil {
		_ = handle.close()
		t.Fatal("accepted another computer block device")
	}
	body, err := os.ReadFile(filepath.Join(restored, "home/agent/history"))
	if err != nil || string(body[:min(len(body), 14)]) != "before capture" {
		t.Fatalf("restored dirty mmap = %q, %v", body, err)
	}
	for _, name := range []string{"run/helmr/authority", "opt/helmr/program/tool"} {
		if _, err := os.Stat(filepath.Join(restored, name)); !os.IsNotExist(err) {
			t.Fatalf("reserved bytes persisted at %s: %v", name, err)
		}
	}
	for path, expected := range frozenWriters {
		relative, err := filepath.Rel(view, path)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(filepath.Join(restored, relative))
		if err != nil || string(actual) != string(expected) {
			t.Fatalf("writer snapshot mismatch at %s: %v", relative, err)
		}
	}
	target, err := os.Readlink(filepath.Join(restored, "home/agent/tool"))
	if err != nil || target != "/opt/helmr/program/tool" {
		t.Fatalf("link = %q, %v", target, err)
	}
	computerProofOwnerLoss(t, filesystem)
	t.Log("parent/child cgroup freeze, mounted ext4 flush/freeze, dirty mmap capture, thaw, independent disk remount, reserved-file exclusion and symlink retention passed")
}

func ext4ProofCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v %s", name, err, output)
	}
}

func computerLoopMount(t *testing.T, disk string) (string, *os.File) {
	t.Helper()
	control, err := os.OpenFile("/dev/loop-control", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	number, err := unix.IoctlRetInt(int(control.Fd()), unix.LOOP_CTL_GET_FREE)
	if err != nil {
		t.Fatal(err)
	}
	node := filepath.Join(t.TempDir(), fmt.Sprintf("loop%d", number))
	if err := unix.Mknod(node, unix.S_IFBLK|0600, int(unix.Mkdev(7, uint32(number)))); err != nil {
		t.Fatal(err)
	}
	loop, err := os.OpenFile(node, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	backing, err := os.OpenFile(disk, os.O_RDWR, 0)
	if err != nil {
		_ = loop.Close()
		t.Fatal(err)
	}
	// LOOP_CONFIGURE is atomic: if another owner wins GET_FREE, EBUSY fails
	// without altering its device. Never clear or reconfigure an existing loop.
	err = unix.IoctlLoopConfigure(int(loop.Fd()), &unix.LoopConfig{Fd: uint32(backing.Fd()), Info: unix.LoopInfo64{Flags: unix.LO_FLAGS_AUTOCLEAR}})
	_ = backing.Close()
	if err != nil {
		_ = loop.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := loop.Close(); err != nil {
			t.Error(err)
		}
	})
	root := t.TempDir()
	if err := unix.Mount(node, root, "ext4", unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount(root, 0); err != nil {
			t.Error(err)
		}
	})
	return root, loop
}

// The fixture parent retains the recovery handle; only the disposable freeze
// owner is killed. No customer processes remain, so cleanup cannot resume work.
// This is not the production recovery policy, which must terminate the VM.
func computerProofOwnerLoss(t *testing.T, filesystem *computerFilesystem) {
	t.Helper()
	ready, signal, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	defer signal.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestComputerFilesystemOwnerLoss$", "-test.v")
	command.Env = append(os.Environ(), "HELMR_FREEZE_OWNER_HELPER=1")
	command.ExtraFiles = []*os.File{filesystem.file, signal}
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	// Parent-death signals are tied to the thread that starts the child.
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		// This independent fixture owner is the only permitted recovery actor.
		err := unix.IoctlSetInt(int(filesystem.file.Fd()), computerThawIOCTL, 0)
		if err != nil && !errors.Is(err, unix.EINVAL) {
			t.Errorf("fixture recovery thaw: %v", err)
		}
	})
	_ = signal.Close()
	readyResult := make(chan error, 1)
	go func() { var b [1]byte; _, err := io.ReadFull(ready, b[:]); readyResult <- err }()
	select {
	case err := <-readyResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("freeze owner did not report ready")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	err = command.Wait()
	exited = true
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("freeze owner was not killed: %v", err)
	}
	// Closing all of the dead owner's descriptors did not release its freeze.
	if err := filesystem.freeze(); !errors.Is(err, unix.EBUSY) {
		if err == nil {
			_ = filesystem.thaw()
		}
		t.Fatalf("freeze after owner death = %v, want EBUSY", err)
	}
	if err := unix.IoctlSetInt(int(filesystem.file.Fd()), computerThawIOCTL, 0); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.freeze(); err != nil {
		t.Fatalf("freeze after fixture recovery: %v", err)
	}
	if err := filesystem.thaw(); err != nil {
		t.Fatal(err)
	}
	t.Log("SIGKILL of freeze owner preserves freeze; independent fixture owner recovered after confirmed process exit")
}

func TestComputerFilesystemOwnerLoss(t *testing.T) {
	if os.Getenv("HELMR_FREEZE_OWNER_HELPER") != "1" {
		t.Skip("invoked by mounted ext4 proof with inherited verified mount")
	}
	if err := unix.IoctlSetInt(3, computerFreezeIOCTL, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.Write(4, []byte{1}); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Hour)
	}
}
