//go:build linux

package deployment

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProgramVerifierCommandUsesFixedDescriptorsAndNamespaces(t *testing.T) {
	code := verifierTestFile(t, "code")
	dependencies := verifierTestFile(t, "dependencies")
	result := verifierTestFile(t, "result")
	pidFD := -1
	command := newProgramVerifierCommand(
		context.Background(),
		programVerifierProcessConfig{
			executable: "/proc/self/exe",
			arguments:  []string{"--verifier"},
		},
		17,
		&pidFD,
		code,
		dependencies,
		result,
	)
	if command.Env == nil || len(command.Env) != 0 {
		t.Fatalf("environment = %#v, want a non-nil empty environment", command.Env)
	}
	if command.Stdin != nil {
		t.Fatalf("stdin = %#v, want nil", command.Stdin)
	}
	if len(command.ExtraFiles) != 3 ||
		command.ExtraFiles[programVerifierCodeFD-3] != code ||
		command.ExtraFiles[programVerifierDependencyFD-3] != dependencies ||
		command.ExtraFiles[programVerifierResultFD-3] != result {
		t.Fatalf("extra files = %#v", command.ExtraFiles)
	}
	if command.SysProcAttr == nil {
		t.Fatal("SysProcAttr is nil")
	}
	wantCloneNamespaces := uintptr(
		syscall.CLONE_NEWNET |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWIPC,
	)
	if command.SysProcAttr.Cloneflags != wantCloneNamespaces {
		t.Fatalf(
			"clone flags = %#x, want %#x",
			command.SysProcAttr.Cloneflags,
			wantCloneNamespaces,
		)
	}
	if command.SysProcAttr.Unshareflags != syscall.CLONE_NEWNS {
		t.Fatalf(
			"unshare flags = %#x, want %#x",
			command.SysProcAttr.Unshareflags,
			syscall.CLONE_NEWNS,
		)
	}
	if !command.SysProcAttr.UseCgroupFD || command.SysProcAttr.CgroupFD != 17 {
		t.Fatalf(
			"cgroup placement = (%t, %d), want (true, 17)",
			command.SysProcAttr.UseCgroupFD,
			command.SysProcAttr.CgroupFD,
		)
	}
	if command.SysProcAttr.PidFD != &pidFD {
		t.Fatal("PidFD pointer was not installed")
	}
}

func TestProgramVerifierCgroupLimits(t *testing.T) {
	path := t.TempDir()
	for _, limit := range programVerifierCgroupLimits {
		if err := os.WriteFile(filepath.Join(path, limit.file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cgroup, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cgroup.Close()
	if err := configureProgramVerifierCgroup(int(cgroup.Fd())); err != nil {
		t.Fatal(err)
	}
	for _, limit := range programVerifierCgroupLimits {
		raw, err := os.ReadFile(filepath.Join(path, limit.file))
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != limit.value {
			t.Fatalf("%s = %q, want %q", limit.file, raw, limit.value)
		}
	}
}

func TestProgramVerifierCgroupRejectsUnsafeLeaseIdentity(t *testing.T) {
	for _, identity := range []string{"", "..", "../lease", "lease/name", "lease_name"} {
		t.Run(identity, func(t *testing.T) {
			if _, err := programVerifierCgroupLeaf(identity); err == nil {
				t.Fatalf("lease identity %q was accepted", identity)
			}
		})
	}
}

func TestProgramVerifierCgroupLeafPreservesLeaseIdentity(t *testing.T) {
	leaf, err := programVerifierCgroupLeaf("lease-123")
	if err != nil {
		t.Fatal(err)
	}
	if leaf != "verifier-lease-123" {
		t.Fatalf("leaf = %q", leaf)
	}
}

func TestCreateProgramVerifierCgroupRejectsSymlinkRoot(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "cgroup")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := createProgramVerifierCgroup(linkRoot, "lease-123"); err == nil {
		t.Fatal("symlinked unit cgroup root was accepted")
	}
}

func TestProgramVerifierCgroupCleanupHelpers(t *testing.T) {
	path := t.TempDir()
	killPath := filepath.Join(path, "cgroup.kill")
	if err := os.WriteFile(killPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := killProgramVerifierCgroup(path); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(killPath); err != nil {
		t.Fatal(err)
	} else if string(raw) != "1" {
		t.Fatalf("cgroup.kill = %q, want 1", raw)
	}

	eventsPath := filepath.Join(path, "cgroup.events")
	if err := os.WriteFile(eventsPath, []byte("populated 1\nfrozen 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		temporary := eventsPath + ".new"
		_ = os.WriteFile(temporary, []byte("populated 0\nfrozen 0\n"), 0o644)
		_ = os.Rename(temporary, eventsPath)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := waitProgramVerifierCgroupEmpty(ctx, path); err != nil {
		t.Fatal(err)
	}
}

func TestOpenProgramVerifierSnapshotRequiresReadOnlyIndependentDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snapshot")
	if err := os.WriteFile(path, []byte("abcdef"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if _, err := source.Seek(3, 0); err != nil {
		t.Fatal(err)
	}
	snapshot, err := openProgramVerifierSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var got [3]byte
	if _, err := snapshot.Read(got[:]); err != nil {
		t.Fatal(err)
	}
	if string(got[:]) != "abc" {
		t.Fatalf("snapshot starts with %q, want abc", got)
	}
	if offset, err := source.Seek(0, 1); err != nil {
		t.Fatal(err)
	} else if offset != 3 {
		t.Fatalf("source offset = %d, want 3", offset)
	}

	writable, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer writable.Close()
	if _, err := openProgramVerifierSnapshot(writable); err == nil {
		t.Fatal("writable Artifact descriptor was accepted")
	}
}

func verifierTestFile(t *testing.T, name string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	return file
}
