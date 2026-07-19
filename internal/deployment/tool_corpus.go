package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ToolRegistryMediaType = "application/vnd.helmr.dependency-tool-registry.v0+json"
	ToolCorpusMediaType   = "application/vnd.helmr.dependency-tool-corpus.v0+json"

	maxToolCorpusManifestBytes       = maxToolRegistryBytes
	maxToolCorpusObjects             = 3072
	maxToolCorpusBytes         int64 = 16 << 30
)

type ToolObject struct {
	Digest    string `json:"digest"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
}

type toolCorpusDocument struct {
	Architecture   RuntimeArchitecture `json:"architecture"`
	FormatVersion  int                 `json:"formatVersion"`
	ObjectCount    int                 `json:"objectCount"`
	Objects        []ToolObject        `json:"objects"`
	Registry       ToolObject          `json:"registry"`
	TotalSizeBytes int64               `json:"totalSizeBytes"`
}

type ToolCorpus struct {
	architecture RuntimeArchitecture
	objects      []ToolObject
	raw          []byte
	digest       string
	mu           sync.RWMutex
	directory    *os.File
	identities   map[string]toolCorpusIdentity
	ownerUID     uint32
	ownerGID     uint32
}

type toolCorpusIdentity struct {
	device             uint64
	inode              uint64
	size               int64
	mode               uint32
	uid                uint32
	gid                uint32
	links              uint64
	modifiedSeconds    int64
	modifiedNanosecond int64
	changedSeconds     int64
	changedNanosecond  int64
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

func (c *ToolCorpus) Close() error {
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
	return err
}

func CanonicalToolCorpus(
	registry *ToolRegistry,
	architecture RuntimeArchitecture,
) ([]byte, error) {
	document, err := toolCorpusForArchitecture(registry, architecture)
	if err != nil {
		return nil, err
	}
	return canonicalToolCorpusDocument(document)
}

func ParseToolCorpus(
	raw []byte,
	registry *ToolRegistry,
	architecture RuntimeArchitecture,
) (*ToolCorpus, error) {
	if registry == nil || !registry.authenticated {
		return nil, errors.New("authenticated dependency tool registry is required")
	}
	if len(raw) == 0 || len(raw) > maxToolCorpusManifestBytes {
		return nil, fmt.Errorf(
			"dependency tool corpus manifest size is outside [1,%d]",
			maxToolCorpusManifestBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency tool corpus manifest: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, errors.New("dependency tool corpus manifest is not RFC 8785 canonical JSON")
	}

	var document toolCorpusDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode dependency tool corpus manifest: %w", err)
	}
	if err := ensureEOF(decoder, "dependency tool corpus manifest"); err != nil {
		return nil, err
	}
	if err := validateToolCorpusDocument(document); err != nil {
		return nil, err
	}
	expected, err := CanonicalToolCorpus(registry, architecture)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(raw, expected) {
		return nil, errors.New("dependency tool corpus manifest does not match the registry architecture closure")
	}
	hash := sha256.Sum256(raw)
	return &ToolCorpus{
		architecture: architecture,
		objects:      append([]ToolObject(nil), document.Objects...),
		raw:          append([]byte(nil), raw...),
		digest:       "sha256:" + hex.EncodeToString(hash[:]),
	}, nil
}

func toolCorpusForArchitecture(
	registry *ToolRegistry,
	architecture RuntimeArchitecture,
) (toolCorpusDocument, error) {
	if registry == nil || len(registry.raw) == 0 || !validToolDigest(registry.digest) {
		return toolCorpusDocument{}, errors.New("dependency tool registry is required")
	}
	if err := ValidateRuntimeArchitecture(architecture); err != nil {
		return toolCorpusDocument{}, err
	}
	objects := make([]ToolObject, 0)
	for _, manager := range registry.managers {
		if manager.Architecture == architecture {
			objects = append(objects, toolObject(manager.ManagerClosure))
		}
	}
	for _, toolchain := range registry.toolchains {
		if toolchain.Architecture == architecture {
			objects = append(objects, toolObject(toolchain.ToolchainClosure))
		}
	}
	for _, toolset := range registry.toolsets {
		if toolset.Architecture == architecture {
			objects = append(objects, toolObject(toolset.Artifact))
		}
	}
	objects, total, err := normalizeToolObjects(objects)
	if err != nil {
		return toolCorpusDocument{}, err
	}
	if len(objects) == 0 {
		return toolCorpusDocument{}, fmt.Errorf(
			"dependency tool registry has no physical closure for architecture %q",
			architecture,
		)
	}
	return toolCorpusDocument{
		Architecture:  architecture,
		FormatVersion: ToolsetFormatVersion,
		ObjectCount:   len(objects),
		Objects:       objects,
		Registry: ToolObject{
			Digest:    registry.digest,
			MediaType: ToolRegistryMediaType,
			SizeBytes: int64(len(registry.raw)),
		},
		TotalSizeBytes: total,
	}, nil
}

func validateToolCorpusDocument(document toolCorpusDocument) error {
	if document.FormatVersion != ToolsetFormatVersion {
		return fmt.Errorf(
			"dependency tool corpus formatVersion = %d, want %d",
			document.FormatVersion,
			ToolsetFormatVersion,
		)
	}
	if err := ValidateRuntimeArchitecture(document.Architecture); err != nil {
		return err
	}
	if !validToolDigest(document.Registry.Digest) ||
		document.Registry.MediaType != ToolRegistryMediaType ||
		document.Registry.SizeBytes < 1 ||
		document.Registry.SizeBytes > maxToolRegistryBytes {
		return errors.New("dependency tool corpus registry descriptor is invalid")
	}
	objects, total, err := normalizeToolObjects(document.Objects)
	if err != nil {
		return err
	}
	if len(objects) == 0 ||
		document.ObjectCount != len(objects) ||
		document.TotalSizeBytes != total {
		return errors.New("dependency tool corpus totals are inconsistent")
	}
	for index := range objects {
		if objects[index] != document.Objects[index] {
			return errors.New("dependency tool corpus objects are not in unique digest order")
		}
	}
	return nil
}

func canonicalToolCorpusDocument(document toolCorpusDocument) ([]byte, error) {
	if err := validateToolCorpusDocument(document); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode dependency tool corpus manifest: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency tool corpus manifest: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > maxToolCorpusManifestBytes {
		return nil, fmt.Errorf(
			"dependency tool corpus manifest size is outside [1,%d]",
			maxToolCorpusManifestBytes,
		)
	}
	return canonical, nil
}

func toolRegistryObjects(document toolRegistryDocument) []ToolObject {
	objects := make([]ToolObject, 0, len(document.Managers)+len(document.Toolchains)+len(document.Toolsets))
	for _, manager := range document.Managers {
		objects = append(objects, toolObject(manager.ManagerClosure))
	}
	for _, toolchain := range document.Toolchains {
		objects = append(objects, toolObject(toolchain.ToolchainClosure))
	}
	for _, toolset := range document.Toolsets {
		objects = append(objects, toolObject(toolset.Artifact))
	}
	return objects
}

func validateToolObjectSet(objects []ToolObject) error {
	_, _, err := normalizeToolObjects(objects)
	return err
}

func normalizeToolObjects(objects []ToolObject) ([]ToolObject, int64, error) {
	if len(objects) == 0 {
		return nil, 0, errors.New("dependency tool object set is empty")
	}
	byDigest := make(map[string]ToolObject, len(objects))
	for _, object := range objects {
		if err := validateToolObject(object); err != nil {
			return nil, 0, err
		}
		if existing, ok := byDigest[object.Digest]; ok {
			if existing != object {
				return nil, 0, fmt.Errorf(
					"dependency tool object digest %q has divergent descriptors",
					object.Digest,
				)
			}
			continue
		}
		byDigest[object.Digest] = object
	}
	if len(byDigest) > maxToolCorpusObjects {
		return nil, 0, fmt.Errorf(
			"dependency tool object count exceeds %d",
			maxToolCorpusObjects,
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
		if object.SizeBytes > maxToolCorpusBytes-total {
			return nil, 0, fmt.Errorf(
				"dependency tool object bytes exceed %d",
				maxToolCorpusBytes,
			)
		}
		total += object.SizeBytes
	}
	return normalized, total, nil
}

func validateToolObject(object ToolObject) error {
	if !validToolDigest(object.Digest) {
		return errors.New("dependency tool object digest is invalid")
	}
	switch object.MediaType {
	case ManagerComponentMediaType, ToolchainMediaType, ManagerDependencyToolsMediaType:
	default:
		return fmt.Errorf(
			"dependency tool object mediaType %q is unsupported",
			object.MediaType,
		)
	}
	if object.SizeBytes < 1 || object.SizeBytes > maxToolArtifactBytes {
		return fmt.Errorf(
			"dependency tool object sizeBytes is outside [1,%d]",
			maxToolArtifactBytes,
		)
	}
	return nil
}

func toolObject(artifact ManagerArtifact) ToolObject {
	return ToolObject{
		Digest:    artifact.Digest,
		MediaType: artifact.MediaType,
		SizeBytes: artifact.SizeBytes,
	}
}
