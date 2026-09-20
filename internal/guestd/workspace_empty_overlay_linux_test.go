//go:build linux

package guestd

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestInitializeEmptyWorkspaceRootOnOverlay(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_TMPDIR", root)
	lower := filepath.Join(root, "lower")
	if err := os.MkdirAll(filepath.Join(lower, "workspace", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(lower, "workspace", "nested", "image.txt")
	if err := os.WriteFile(original, []byte("image contents"), 0o644); err != nil {
		t.Fatal(err)
	}
	merged, cleanup, err := createOverlayImageRoot(lower)
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENODEV) {
		t.Skipf("requires OverlayFS mount capability: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	workspace := filepath.Join(merged, "workspace")
	if err := initializeEmptyWorkspaceRoot(workspace); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("workspace is not empty: %v", entries)
	}
	body, err := os.ReadFile(original)
	if err != nil || string(body) != "image contents" {
		t.Fatalf("lower image changed: %q, %v", body, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
}
