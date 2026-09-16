//go:build linux || darwin

package buildcontext

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCaptureRejectsSpecialFiles(t *testing.T) {
	source, temp := t.TempDir(), t.TempDir()
	if err := unix.Mkfifo(filepath.Join(source, "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	if result, err := Capture(t.Context(), source, temp); err == nil {
		result.Close()
		t.Fatal("accepted FIFO")
	}
	children, err := os.ReadDir(temp)
	if err != nil || len(children) != 0 {
		t.Fatal("special file cleanup failed")
	}
}

func TestCaptureFileReplacedByFIFODoesNotBlock(t *testing.T) {
	source := t.TempDir()
	name := filepath.Join(source, "file")
	writeTestFile(t, name, "content")
	snapshot := collectTestSourceSnapshot(t, source)
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(name, 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.writeFile(t.Context(), io.Discard, snapshot.entries[0]); !errors.Is(err, errSourceChanged) {
		t.Fatalf("FIFO replacement: %v", err)
	}
}
