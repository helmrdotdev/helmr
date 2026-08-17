package imagebuild

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	MaxSourceArchiveBytes   = int64(11 << 30)
	MaxSourceArchiveEntries = 100000
)

// SourceArchiveDescriptor describes the exact installed-tree source projection
// used by the local bundle producer when it constructs a Workspace image.
// It is producer-local metadata and never becomes Control Plane build authority.
type SourceArchiveDescriptor struct {
	ArchiveDigest    string
	ArchiveSizeBytes int64
	ArchiveEntries   int
	PathSetDigest    string
}

type SourcePath struct {
	Path string         `json:"path"`
	Kind SourcePathKind `json:"kind"`
}

type SourcePathKind string

const (
	SourcePathFile      SourcePathKind = "file"
	SourcePathDirectory SourcePathKind = "directory"
	SourcePathSymlink   SourcePathKind = "symlink"
)

func PathSetDigest(paths []SourcePath) string {
	raw, err := json.Marshal(paths)
	if err != nil {
		panic(err)
	}
	return sha256sum.DigestBytes(raw)
}
