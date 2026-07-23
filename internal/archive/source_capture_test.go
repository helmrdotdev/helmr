package archive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceSnapshotRejectsEntryMutation(t *testing.T) {
	rootPath := t.TempDir()
	filePath := filepath.Join(rootPath, "task.ts")
	writeTestFile(t, filePath, "before")
	snapshot := collectTestSourceSnapshot(t, rootPath)
	writeTestFile(t, filePath, "after!")
	if err := snapshot.verify(); !errors.Is(err, errSourceChanged) {
		t.Fatalf("verify mutation error = %v, want source changed", err)
	}
}

func TestSourceSnapshotRejectsMembershipMutation(t *testing.T) {
	rootPath := t.TempDir()
	writeTestFile(t, filepath.Join(rootPath, "task.ts"), "task")
	snapshot := collectTestSourceSnapshot(t, rootPath)
	writeTestFile(t, filepath.Join(rootPath, "added.ts"), "added")
	if err := snapshot.verify(); !errors.Is(err, errSourceChanged) {
		t.Fatalf("verify membership error = %v, want source changed", err)
	}
}

func TestSourceSnapshotRejectsSymlinkMutation(t *testing.T) {
	rootPath := t.TempDir()
	linkPath := filepath.Join(rootPath, "link")
	if err := os.Symlink("first", linkPath); err != nil {
		t.Fatal(err)
	}
	snapshot := collectTestSourceSnapshot(t, rootPath)
	if err := os.Remove(linkPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("second", linkPath); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.verify(); !errors.Is(err, errSourceChanged) {
		t.Fatalf("verify symlink error = %v, want source changed", err)
	}
}

func collectTestSourceSnapshot(t *testing.T, rootPath string) *sourceSnapshot {
	t.Helper()
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	snapshot := &sourceSnapshot{
		root:        root,
		observed:    make(map[string]sourceObserved),
		directories: make(map[string][]string),
	}
	if err := snapshot.collect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return snapshot
}
