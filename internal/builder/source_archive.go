package builder

import (
	"encoding/json"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

const (
	maxSourceArchiveBytes   = int64(11 << 30)
	maxSourceArchiveEntries = 100000
)

// sourceArchiveDescriptor describes the exact installed-tree source projection
// used by the local bundle producer when it constructs a Computer image.
// It is producer-local metadata and never becomes Control Plane build authority.
type sourceArchiveDescriptor struct {
	ArchiveDigest    string
	ArchiveSizeBytes int64
	ArchiveEntries   int
	PathSetDigest    string
}

type sourceArchivePath struct {
	Path string         `json:"path"`
	Kind sourcePathKind `json:"kind"`
}

type sourcePathKind string

const (
	sourcePathFile      sourcePathKind = "file"
	sourcePathDirectory sourcePathKind = "directory"
	sourcePathSymlink   sourcePathKind = "symlink"
)

func sourcePathSetDigest(paths []sourceArchivePath) string {
	raw, err := json.Marshal(paths)
	if err != nil {
		panic(err)
	}
	return sha256sum.DigestBytes(raw)
}
