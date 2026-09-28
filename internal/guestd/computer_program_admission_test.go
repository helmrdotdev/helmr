package guestd

import (
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testProgramMount(t *testing.T) (*computerMountEntry, *computerOperationRegistry, *computerv0.ComputerRunAuthority) {
	t.Helper()
	parent := t.TempDir()
	computerRoot := filepath.Join(parent, "computer")
	if err := os.Mkdir(computerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(computerRoot, "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := testComputerAuthorityEntry()
	entry.computerRoot = computerRoot
	authority := testComputerRunAuthority(time.Now().Add(time.Minute))
	registry := newComputerOperationRegistry()
	registry.register("runtime-1", entry)
	t.Cleanup(func() { registry.retire("runtime-1", entry) })
	return entry, registry, authority
}

func TestComputerProgramAdmissionRejectsRetiredMount(t *testing.T) {
	entry, registry, authority := testProgramMount(t)
	replacement := testComputerAuthorityEntry()
	registry.register("runtime-1", replacement)
	if _, err := registry.admitProgram(entry, authority, time.Now); err == nil {
		t.Fatal("Program was admitted on a retired Mount")
	}
}

func TestComputerProgramAdmissionCannotChangeInstanceWriter(t *testing.T) {
	entry, registry, authority := testProgramMount(t)
	before := entry.currentWriterGeneration()
	authority.GetFence().WriterGeneration++
	if release, err := registry.admitProgram(entry, authority, time.Now); err == nil {
		release()
		t.Fatal("Run changed physical writer")
	}
	if entry.currentWriterGeneration() != before {
		t.Fatal("writer changed")
	}
}
