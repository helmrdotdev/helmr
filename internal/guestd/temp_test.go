package guestd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkdirGuestdTempUsesConfiguredDiskBackedRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HELMR_GUESTD_TMPDIR", root)

	dir, err := mkdirGuestdTemp("helmr-test-*")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, root+string(filepath.Separator)) {
		t.Fatalf("temp dir = %q, want under %q", dir, root)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat temp dir: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("temp path is not a directory: %s", dir)
	}
}
