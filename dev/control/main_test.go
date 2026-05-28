package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationPathsFindsSourceRootWhenCwdDiffers(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	paths, err := migrationPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("expected migration paths")
	}
	for _, path := range paths {
		if filepath.IsAbs(path) {
			continue
		}
		t.Fatalf("expected fallback migration path to be absolute, got %q", path)
	}
}
