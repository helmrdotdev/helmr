package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPayloadAcceptsLargeDormantInstalledBinary(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaxProgramFileSizeBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProgramPayloadDigest(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadRejectsInvalidLinkBytesBeforeHash(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(string([]byte{0xff}), filepath.Join(root, "bad-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ProgramPayloadDigest(t.Context(), root); err == nil {
		t.Fatal("accepted invalid link bytes")
	}
}
