package artifacttest

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

// Digest is the SHA-256 digest of value.
func Digest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return sha256sum.FormatDigest(digest[:])
}

func exactFilesystem() artifact.Filesystem {
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

// Memory is an in-memory SquashFS artifact. Files holds regular file bodies by
// path; writing it directly leaves the recorded entry unchanged.
type Memory struct {
	Files      map[string][]byte
	entries    []artifact.Entry
	nextInode  uint64
	filesystem artifact.Filesystem
}

// NewMemory returns an artifact holding only its root directory.
func NewMemory() *Memory {
	memory := &Memory{
		Files:      make(map[string][]byte),
		nextInode:  2,
		filesystem: exactFilesystem(),
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

func (memory *Memory) Filesystem() artifact.Filesystem {
	filesystem := memory.filesystem
	filesystem.IDs = append([]uint32(nil), filesystem.IDs...)
	return filesystem
}

func (memory *Memory) Entries(context.Context) ([]artifact.Entry, error) {
	return append([]artifact.Entry(nil), memory.entries...), nil
}

func (memory *Memory) Open(_ context.Context, path string) (io.ReadCloser, error) {
	raw, exists := memory.Files[path]
	if !exists {
		return nil, fmt.Errorf("file %q is absent", path)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (memory *Memory) AddDirectory(path string) {
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

func (memory *Memory) AddFile(path string, raw []byte, mode uint32) {
	memory.Files[path] = append([]byte(nil), raw...)
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

func (memory *Memory) AddLink(path, target string) {
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

// Mutate edits the entry at path in place; the entry must exist.
func (memory *Memory) Mutate(path string, mutate func(*artifact.Entry)) {
	for position := range memory.entries {
		if memory.entries[position].Path == path {
			mutate(&memory.entries[position])
			return
		}
	}
	panic("entry is absent: " + path)
}

// ReplaceFile replaces the body at path and records its new size.
func (memory *Memory) ReplaceFile(path string, raw []byte) {
	memory.Files[path] = raw
	memory.Mutate(path, func(entry *artifact.Entry) { entry.SizeBytes = int64(len(raw)) })
}

// Remove drops the entry at path and its body, leaving the inode count as is.
// An absent path is left alone.
func (memory *Memory) Remove(path string) {
	for index := range memory.entries {
		if memory.entries[index].Path != path {
			continue
		}
		memory.entries = append(memory.entries[:index], memory.entries[index+1:]...)
		delete(memory.Files, path)
		return
	}
}

func (memory *Memory) takeInode() uint64 {
	inode := memory.nextInode
	memory.nextInode++
	memory.filesystem.InodeCount++
	return inode
}
