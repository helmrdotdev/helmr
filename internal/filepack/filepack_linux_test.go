//go:build linux

package filepack

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

	stats, err := Pack(context.Background(), source, target, ScratchRole)
	if err != nil {
		t.Fatal(err)
	}
	if stats.LogicalBytes != 64<<20 || stats.EncodedChunks == 0 {
		t.Fatalf("pack stats = %+v", stats)
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
	restoreStats, err := Unpack(context.Background(), target, restored, ScratchRole, sourceInfo.Size())
	if err != nil {
		t.Fatal(err)
	}
	if restoreStats.LogicalBytes != sourceInfo.Size() || restoreStats.UnpackWrittenBytes == 0 || restoreStats.EncodedChunks != stats.EncodedChunks {
		t.Fatalf("unpack stats = %+v pack=%+v", restoreStats, stats)
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
	if _, err := Pack(context.Background(), source, target, MemoryRole); err != nil {
		t.Fatal(err)
	}
	_, err := Unpack(context.Background(), target, filepath.Join(dir, "restored.raw"), ScratchRole, int64(len("memory")))
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
	if _, err := Pack(context.Background(), source, target, MemoryRole); err != nil {
		t.Fatal(err)
	}
	_, err := Unpack(context.Background(), target, filepath.Join(dir, "restored.raw"), MemoryRole, 1<<20)
	if err == nil {
		t.Fatal("unpack succeeded with mismatched logical size")
	}
}

func TestRuntimeFilepackRejectsOverflowingDataRecord(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.raw")
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	compressed := encoder.EncodeAll([]byte("abcd"), nil)
	var record bytes.Buffer
	var header [20]byte
	binary.BigEndian.PutUint64(header[:8], uint64(maxInt64-1))
	binary.BigEndian.PutUint32(header[8:12], 4)
	binary.BigEndian.PutUint64(header[12:20], uint64(len(compressed)))
	record.Write(header[:])
	record.Write(compressed)

	var nextOffset int64
	err = readFilepackDataRecord(&record, file, decoder, nil, maxInt64, &nextOffset)
	if err == nil || !strings.Contains(err.Error(), "invalid Firecracker filepack data record") {
		t.Fatalf("err = %v, want invalid Firecracker filepack data record", err)
	}
}

func TestScanAndWriteFilepackRangeRejectsShortRead(t *testing.T) {
	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.raw")
	targetPath := filepath.Join(dir, "target.filepack")
	if err := os.WriteFile(sourcePath, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, err := os.Create(targetPath)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()

	err = scanAndWriteFilepackRange(context.Background(), source, target, encoder, nil, 0, 16)
	if err == nil {
		t.Fatal("scan succeeded with short read")
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

func TestPackIndependentOfPhysicalAllocation(t *testing.T) {
	dir := t.TempDir()
	const size = 3*filepackChunkSize + 4096
	content := make([]byte, size)
	offsets := []int64{5000, 16384, filepackChunkSize - 1, filepackChunkSize + 8192, size - 1}
	for _, offset := range offsets {
		content[offset] = byte(offset%251 + 1)
	}
	dense, sparse := filepath.Join(dir, "dense"), filepath.Join(dir, "sparse")
	if err := os.WriteFile(dense, content, 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for _, offset := range offsets {
		if _, err := f.WriteAt(content[offset:offset+1], offset); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a.pack"), filepath.Join(dir, "b.pack")
	if _, err := Pack(t.Context(), dense, a, ScratchRole); err != nil {
		t.Fatal(err)
	}
	if _, err := Pack(t.Context(), sparse, b, ScratchRole); err != nil {
		t.Fatal(err)
	}
	x, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(x, y) {
		t.Fatal("identical logical bytes encoded differently according to physical allocation")
	}
	restored := filepath.Join(dir, "restored")
	if _, err := Unpack(t.Context(), b, restored, ScratchRole, size); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(restored)
	if err != nil || !bytes.Equal(actual, content) {
		t.Fatalf("restored bytes differ: %v", err)
	}
}

func TestUnpackRejectsUnboundedRecords(t *testing.T) {
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	chunk := encoder.EncodeAll(bytes.Repeat([]byte{1}, int(filepackChunkSize)), nil)
	for _, kind := range []string{"duplicate", "descending", "unaligned", "short", "expansion"} {
		t.Run(kind, func(t *testing.T) {
			var stream bytes.Buffer
			logical := 2 * filepackChunkSize
			if kind == "expansion" {
				logical = 4096
			}
			if err := writeFilepackHeader(&stream, filepackHeader{Version: filepackVersion, Role: ScratchRole, LogicalSize: logical, ChunkSize: filepackChunkSize, Codec: filepackCodecZstd}); err != nil {
				t.Fatal(err)
			}
			write := func(offset int64, size int) {
				t.Helper()
				if err := writeFilepackDataRecord(&stream, offset, size, chunk); err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "duplicate":
				write(0, int(filepackChunkSize))
				write(0, int(filepackChunkSize))
			case "descending":
				write(filepackChunkSize, int(filepackChunkSize))
				write(0, int(filepackChunkSize))
			case "unaligned":
				write(1, int(filepackChunkSize))
			case "short":
				write(0, 1)
			case "expansion":
				write(0, 4096)
			}
			stream.WriteByte(filepackRecordEnd)
			target := filepath.Join(t.TempDir(), "disk")
			if _, err := UnpackFrom(t.Context(), &stream, target, ScratchRole, logical); err == nil {
				t.Fatal("malformed record accepted")
			}
			if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Fatalf("failed output retained: %v", err)
			}
		})
	}
}
