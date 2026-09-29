package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func exactTestFilesystem() artifact.Filesystem {
	return artifact.Filesystem{
		Magic:              artifact.SquashFSMagic,
		InodeCount:         1,
		BlockSize:          artifact.SquashFSDataBlockSize,
		Compressor:         artifact.SquashFSZstandardCompressor,
		BlockLog:           17,
		Flags:              artifact.SquashFSV0Flags,
		IDCount:            1,
		Major:              4,
		Minor:              0,
		RootInodeReference: 1,
		BytesUsed:          artifact.SquashFSSuperblockSize,
		PhysicalSize:       artifact.SquashFSPhysicalAlign,
		XattrIDTableStart:  math.MaxUint64,
		ExportTableStart:   math.MaxUint64,
		IDs:                []uint32{0},
		HasZeroPadding:     true,
	}
}

type memoryArtifact struct {
	files      map[string][]byte
	entries    []artifact.Entry
	nextInode  uint64
	filesystem artifact.Filesystem
}

func newMemoryArtifact() *memoryArtifact {
	memory := &memoryArtifact{
		files:      make(map[string][]byte),
		nextInode:  2,
		filesystem: exactTestFilesystem(),
	}
	memory.entries = append(memory.entries, artifact.Entry{
		Path:        ".",
		Kind:        artifact.EntryDirectory,
		Form:        artifact.SquashFSBasicDirectoryForm,
		Mode:        0755,
		XattrIndex:  artifact.SquashFSInvalidXattr,
		Inode:       1,
		InodeNumber: 1,
	})
	return memory
}

func (memory *memoryArtifact) Filesystem() artifact.Filesystem {
	filesystem := memory.filesystem
	filesystem.IDs = append([]uint32(nil), filesystem.IDs...)
	return filesystem
}

func (memory *memoryArtifact) Entries(context.Context) ([]artifact.Entry, error) {
	return append([]artifact.Entry(nil), memory.entries...), nil
}

func (memory *memoryArtifact) Open(_ context.Context, path string) (io.ReadCloser, error) {
	raw, exists := memory.files[path]
	if !exists {
		return nil, fmt.Errorf("file %q is absent", path)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (memory *memoryArtifact) addDirectory(path string) {
	inode := memory.takeInode()
	memory.entries = append(memory.entries, artifact.Entry{
		Path:        path,
		Kind:        artifact.EntryDirectory,
		Form:        artifact.SquashFSBasicDirectoryForm,
		Mode:        0755,
		XattrIndex:  artifact.SquashFSInvalidXattr,
		Inode:       inode,
		InodeNumber: uint32(inode),
	})
}

func (memory *memoryArtifact) addFile(path string, raw []byte, mode uint32) {
	memory.files[path] = append([]byte(nil), raw...)
	inode := memory.takeInode()
	memory.entries = append(memory.entries, artifact.Entry{
		Path:        path,
		Kind:        artifact.EntryRegular,
		Form:        artifact.SquashFSBasicRegularForm,
		Mode:        mode,
		SizeBytes:   int64(len(raw)),
		XattrIndex:  artifact.SquashFSInvalidXattr,
		Inode:       inode,
		InodeNumber: uint32(inode),
		LinkCount:   1,
	})
}

func (memory *memoryArtifact) addLink(path, target string) {
	inode := memory.takeInode()
	memory.entries = append(memory.entries, artifact.Entry{
		Path:        path,
		Kind:        artifact.EntrySymlink,
		Form:        artifact.SquashFSBasicSymlinkForm,
		Mode:        0777,
		SizeBytes:   int64(len(target)),
		XattrIndex:  artifact.SquashFSInvalidXattr,
		LinkTarget:  target,
		Inode:       inode,
		InodeNumber: uint32(inode),
		LinkCount:   1,
	})
}

func (memory *memoryArtifact) mutate(path string, mutate func(*artifact.Entry)) {
	for position := range memory.entries {
		if memory.entries[position].Path == path {
			mutate(&memory.entries[position])
			return
		}
	}
	panic("entry is absent: " + path)
}

func (memory *memoryArtifact) takeInode() uint64 {
	inode := memory.nextInode
	memory.nextInode++
	memory.filesystem.InodeCount++
	return inode
}

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return sha256sum.FormatDigest(digest[:])
}

func (memory *memoryArtifact) replaceFile(path string, raw []byte) {
	memory.files[path] = raw
	memory.mutate(path, func(entry *artifact.Entry) { entry.SizeBytes = int64(len(raw)) })
}
