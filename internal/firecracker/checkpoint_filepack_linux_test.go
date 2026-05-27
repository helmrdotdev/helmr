//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeFilepackRoundTripsSparseFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	target := filepath.Join(dir, "source.filepack")
	restored := filepath.Join(dir, "restored.raw")
	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("begin"), 4096); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(bytes.Repeat([]byte{0x5a}, 1024), 40<<20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := packRuntimeFile(context.Background(), source, target, filepackScratchRole); err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	targetInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if targetInfo.Size() >= sourceInfo.Size()/8 {
		t.Fatalf("packed sparse file size = %d, source = %d", targetInfo.Size(), sourceInfo.Size())
	}
	if err := unpackRuntimeFile(context.Background(), target, restored, filepackScratchRole, sourceInfo.Size()); err != nil {
		t.Fatal(err)
	}
	assertFileByteRange(t, restored, 4096, []byte("begin"))
	assertFileByteRange(t, restored, 40<<20, bytes.Repeat([]byte{0x5a}, 1024))
	restoredInfo, err := os.Stat(restored)
	if err != nil {
		t.Fatal(err)
	}
	if restoredInfo.Size() != sourceInfo.Size() {
		t.Fatalf("restored size = %d, want %d", restoredInfo.Size(), sourceInfo.Size())
	}
}

func TestRuntimeFilepackRejectsRoleMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	target := filepath.Join(dir, "source.filepack")
	if err := os.WriteFile(source, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := packRuntimeFile(context.Background(), source, target, filepackMemoryRole); err != nil {
		t.Fatal(err)
	}
	err := unpackRuntimeFile(context.Background(), target, filepath.Join(dir, "restored.raw"), filepackScratchRole, int64(len("memory")))
	if err == nil {
		t.Fatal("unpack succeeded with mismatched role")
	}
}

func TestRuntimeFilepackRejectsLogicalSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	target := filepath.Join(dir, "source.filepack")
	if err := os.WriteFile(source, []byte("memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := packRuntimeFile(context.Background(), source, target, filepackMemoryRole); err != nil {
		t.Fatal(err)
	}
	err := unpackRuntimeFile(context.Background(), target, filepath.Join(dir, "restored.raw"), filepackMemoryRole, 1<<20)
	if err == nil {
		t.Fatal("unpack succeeded with mismatched logical size")
	}
}

func assertFileByteRange(t *testing.T, path string, offset int64, want []byte) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got := make([]byte, len(want))
	if _, err := file.ReadAt(got, offset); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("bytes at %d = %x, want %x", offset, got, want)
	}
}
