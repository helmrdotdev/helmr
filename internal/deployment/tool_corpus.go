package deployment

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
)

const (
	maxToolchainCorpusObjects       = maxToolchainCatalogMembers
	maxToolchainCorpusBytes   int64 = 16 << 30
)

type ToolObject struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ToolchainCorpus struct {
	//lint:ignore U1000 read by the Linux-only corpus verifier
	architecture RuntimeArchitecture
	toolchains   map[string]Toolchain
	mu           sync.RWMutex
	directory    *os.File
	identities   map[string]toolCorpusIdentity
	//lint:ignore U1000 read by the Linux-only corpus verifier
	ownerUID uint32
	//lint:ignore U1000 read by the Linux-only corpus verifier
	ownerGID uint32
}

type toolCorpusIdentity struct {
	//lint:ignore U1000 read by the Linux-only corpus verifier
	device, inode, links uint64
	//lint:ignore U1000 read by the Linux-only corpus verifier
	size, modifiedSeconds, modifiedNanosecond, changedSeconds, changedNanosecond int64
	//lint:ignore U1000 read by the Linux-only corpus verifier
	mode, uid, gid uint32
}

type ToolObjectFile struct {
	file       *os.File
	descriptor ToolObject
}

func (f *ToolObjectFile) Descriptor() ToolObject {
	if f == nil {
		return ToolObject{}
	}
	return f.descriptor
}

func (f *ToolObjectFile) File() *os.File {
	if f == nil {
		return nil
	}
	return f.file
}

func (f *ToolObjectFile) Close() error {
	if f == nil || f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

func (c *ToolchainCorpus) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.directory == nil {
		return nil
	}
	err := c.directory.Close()
	c.directory = nil
	c.identities = nil
	c.toolchains = nil
	return err
}

func toolchainClosureObjects(
	catalog *ToolchainCatalog,
	architecture RuntimeArchitecture,
) ([]ToolObject, error) {
	if catalog == nil {
		return nil, errors.New("standard-toolchain catalog is required")
	}
	if architecture != "" {
		if err := ValidateRuntimeArchitecture(architecture); err != nil {
			return nil, err
		}
	}
	objects := make([]ToolObject, 0, len(catalog.toolchains))
	for _, toolchain := range catalog.toolchains {
		if architecture == "" || toolchain.Architecture == architecture {
			objects = append(objects, toolObject(toolchain.ToolchainClosure))
		}
	}
	objects, err := normalizeToolObjects(objects)
	if err != nil {
		return nil, err
	}
	for _, object := range objects {
		if object.MediaType != ToolchainMediaType {
			return nil, fmt.Errorf(
				"standard-toolchain closure %q mediaType = %q, want %q",
				object.Digest,
				object.MediaType,
				ToolchainMediaType,
			)
		}
	}
	return objects, nil
}

func normalizeToolObjects(objects []ToolObject) ([]ToolObject, error) {
	if len(objects) == 0 {
		return nil, errors.New("standard-toolchain object set is empty")
	}
	byDigest := make(map[string]ToolObject, len(objects))
	for _, object := range objects {
		if err := validateToolObject(object); err != nil {
			return nil, err
		}
		if existing, ok := byDigest[object.Digest]; ok {
			if existing != object {
				return nil, fmt.Errorf(
					"standard-toolchain object digest %q has divergent descriptors",
					object.Digest,
				)
			}
			continue
		}
		byDigest[object.Digest] = object
	}
	if len(byDigest) > maxToolchainCorpusObjects {
		return nil, fmt.Errorf(
			"standard-toolchain object count exceeds %d",
			maxToolchainCorpusObjects,
		)
	}
	normalized := make([]ToolObject, 0, len(byDigest))
	for _, object := range byDigest {
		normalized = append(normalized, object)
	}
	sort.Slice(normalized, func(left, right int) bool {
		return normalized[left].Digest < normalized[right].Digest
	})
	var total int64
	for _, object := range normalized {
		if object.SizeBytes > maxToolchainCorpusBytes-total {
			return nil, fmt.Errorf(
				"standard-toolchain object bytes exceed %d",
				maxToolchainCorpusBytes,
			)
		}
		total += object.SizeBytes
	}
	return normalized, nil
}

func validateToolObject(object ToolObject) error {
	if !validToolDigest(object.Digest) {
		return errors.New("standard-toolchain object digest is invalid")
	}
	if object.MediaType != ToolchainMediaType {
		return fmt.Errorf(
			"standard-toolchain object mediaType %q is unsupported",
			object.MediaType,
		)
	}
	if object.SizeBytes < 1 || object.SizeBytes > maxToolArtifactBytes {
		return fmt.Errorf(
			"standard-toolchain object sizeBytes is outside [1,%d]",
			maxToolArtifactBytes,
		)
	}
	return nil
}

func toolObject(artifact ArtifactDescriptor) ToolObject {
	return ToolObject(artifact)
}
