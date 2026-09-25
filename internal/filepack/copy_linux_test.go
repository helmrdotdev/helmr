//go:build linux

package filepack

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneSparseFilePreservesSparseExtents(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.raw")
	dest := filepath.Join(dir, "dest.raw")
	const logicalSize = int64(64 << 20)
	const dataOffset = int64(32 << 20)
	payload := bytes.Repeat([]byte("x"), 4096)

	file, err := os.OpenFile(source, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(logicalSize); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt(payload, dataOffset); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Copy(source, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != logicalSize {
		t.Fatalf("dest size = %d, want %d", info.Size(), logicalSize)
	}
	destFile, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer destFile.Close()
	read := make([]byte, len(payload))
	if _, err := destFile.ReadAt(read, dataOffset); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, payload) {
		t.Fatalf("copied payload mismatch")
	}
	if allocatedBytes(t, dest) > logicalSize/8 {
		t.Fatalf("dest was copied densely: allocated=%d logical=%d", allocatedBytes(t, dest), logicalSize)
	}
}

func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Blocks * 512
}

func TestCopySparseRangeRejectsShortRead(t *testing.T) {
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.raw")
	outputPath := filepath.Join(dir, "output.raw")
	if err := os.WriteFile(inputPath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := os.OpenFile(outputPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	buffer := bytes.Repeat([]byte{0xff}, 16)

	if err := copySparseRange(input, output, buffer, 0, 16); err == nil {
		t.Fatal("copy succeeded with short read")
	}
}
