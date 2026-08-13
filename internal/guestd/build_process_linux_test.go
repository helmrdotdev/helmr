//go:build linux

package guestd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestBuildProcessProcMountUsesConformanceRestrictions(t *testing.T) {
	want := uintptr(unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if got := buildProcMountFlags(); got != want {
		t.Fatalf("proc mount flags = %#x, want %#x", got, want)
	}
}

func TestBuildProcessProcMountProvidesRestrictedProcessSurface(t *testing.T) {
	const child = "HELMR_TEST_BUILD_PROC_CHILD"
	if os.Getenv(child) == "1" {
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "proc"), 0o555); err != nil {
			t.Fatal(err)
		}
		if err := mountBuildProc(root); err != nil {
			t.Fatal(err)
		}
		procPath := filepath.Join(root, "proc")
		defer func() {
			if err := unix.Unmount(procPath, unix.MNT_DETACH); err != nil {
				t.Errorf("unmount isolated build procfs: %v", err)
			}
		}()
		for _, name := range []string{"self", "self/fd", "stat", "self/cgroup"} {
			if _, err := os.Stat(filepath.Join(procPath, name)); err != nil {
				t.Fatalf("stat procfs path %q: %v", name, err)
			}
		}
		var state unix.Statfs_t
		if err := unix.Statfs(procPath, &state); err != nil {
			t.Fatal(err)
		}
		want := int64(unix.ST_RDONLY | unix.ST_NOSUID | unix.ST_NODEV | unix.ST_NOEXEC)
		if state.Type != unix.PROC_SUPER_MAGIC || state.Flags&want != want {
			t.Fatalf("procfs type/flags = %#x/%#x, want type %#x and flags %#x", state.Type, state.Flags, unix.PROC_SUPER_MAGIC, want)
		}
		file, err := os.OpenFile(filepath.Join(procPath, "sys/kernel/hostname"), os.O_WRONLY, 0)
		if err == nil {
			_ = file.Close()
			t.Fatal("read-only build procfs admitted a writable open")
		}
		if !errors.Is(err, syscall.EROFS) && !errors.Is(err, syscall.EACCES) && !errors.Is(err, syscall.EPERM) {
			t.Fatalf("writable procfs open failed with %v", err)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("the procfs behavior test requires root in a private mount namespace")
	}
	command := exec.Command(os.Args[0], "-test.run=^TestBuildProcessProcMountProvidesRestrictedProcessSurface$")
	command.Env = append(os.Environ(), child+"=1")
	command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run isolated procfs probe: %v\n%s", err, output)
	}
}

func TestBuildOutputRetainsOneCombinedBound(t *testing.T) {
	output := &buildOutput{limit: 8}
	output.append([]byte("12345"), true)
	output.append([]byte("678"), false)
	result := output.result(nil)
	if result.overflow {
		t.Fatal("output at the exact bound was marked truncated")
	}
	if got := append(result.stdout, result.stderr...); !bytes.Equal(
		got,
		[]byte("12345678"),
	) {
		t.Fatalf("retained output = %q", got)
	}
}

func TestBuildOutputDrainsAndMarksTruncation(t *testing.T) {
	limit := len(buildOutputMarker) + 4
	output := &buildOutput{limit: limit}
	output.append(bytes.Repeat([]byte("s"), limit), true)
	output.append([]byte("stderr"), false)
	result := output.result(nil)
	if !result.overflow {
		t.Fatal("output above the bound was not marked truncated")
	}
	combined := append(
		append([]byte(nil), result.stdout...),
		result.stderr...,
	)
	if len(combined) != limit {
		t.Fatalf("retained output size = %d, want %d", len(combined), limit)
	}
	if !bytes.HasSuffix(combined, []byte(buildOutputMarker)) {
		t.Fatalf("retained output lacks truncation marker: %q", combined)
	}
}
