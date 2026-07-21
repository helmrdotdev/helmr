package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonicalEmptyTreeDigest(t *testing.T) {
	sum := sha256.Sum256([]byte(TreeDigestDomain))
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != CanonicalEmptyTreeDigest {
		t.Fatalf("empty tree digest = %q, want %q", got, CanonicalEmptyTreeDigest)
	}
}

func TestInspectTreeCanonicalIdentity(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "run"), []byte("hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "bin", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("bin/run", filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}

	identity, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := identity.Digest, "sha256:0ee677d67a02c8ea3947748dfcd278e9c25fdd956e5de8af97c16f8bd922f253"; got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	if identity.SizeBytes != 6 || identity.EntryCount != 3 {
		t.Fatalf("identity = %#v", identity)
	}

	now := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "bin", "run"), now, now); err != nil {
		t.Fatal(err)
	}
	afterTimeChange, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if afterTimeChange != identity {
		t.Fatalf("mtime changed identity from %#v to %#v", identity, afterTimeChange)
	}
}

func TestInspectTreeIncludesPermissionsAndFileBytes(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	initial, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if modeChanged.Digest == initial.Digest {
		t.Fatal("permission change did not change digest")
	}
	if err := os.WriteFile(path, []byte("other"), 0o700); err != nil {
		t.Fatal(err)
	}
	contentChanged, err := InspectTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if contentChanged.Digest == modeChanged.Digest {
		t.Fatal("content change did not change digest")
	}
}

func TestInspectTreeRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectTree(root); err == nil {
		t.Fatal("expected escaping symlink to fail")
	}
}
