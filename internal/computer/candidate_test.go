package computer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCandidateCloseClosesHandleWhenUnlinkFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := &DiskCandidate{file: file}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(path, "busy")
	if err := os.WriteFile(child, []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err == nil {
		t.Fatal("unlink failure lost")
	}
	if _, err := file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("descriptor remains open: %v", err)
	}
	if err := os.Remove(child); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retained path not removed: %v", err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
}
