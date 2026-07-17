package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxArtifactEntries               = 200000
	maxArtifactDepth                 = 128
	maxArtifactFileSize        int64 = 1 << 30
	maxArtifactNameBytes       int64 = 128 << 20
	maxCodeLogicalBytes        int64 = 2 << 30
	maxDependencyLogicalBytes  int64 = 8 << 30
	maxCodePhysicalBytes       int64 = 3 << 30
	maxDependencyPhysicalBytes int64 = 10 << 30
	maxPackageJSONBytes        int64 = 256 << 20
	maxLockfileBytes           int64 = 64 << 20
	maxSymlinkTargetBytes            = 4095
	maxSymlinkHops                   = 40
)

type artifactEntryKind string

const (
	artifactEntryRegular   = artifactEntryKind("regular")
	artifactEntryDirectory = artifactEntryKind("directory")
	artifactEntrySymlink   = artifactEntryKind("symlink")
)

type artifactFilesystem struct {
	Major               uint16
	Minor               uint16
	Compression         string
	BlockSize           uint32
	CreatedAtUnix       int64
	HasFragments        bool
	HasDuplicatePacking bool
	HasOverlappingData  bool
	HasExportTable      bool
	HasXattrs           bool
}

type artifactEntry struct {
	Path        string
	Kind        artifactEntryKind
	Mode        uint32
	SizeBytes   int64
	UID         uint32
	GID         uint32
	ModTimeUnix int64
	LinkTarget  string
	Inode       uint64
	LinkCount   uint32
	HasXattrs   bool
}

type artifactReader interface {
	Filesystem() artifactFilesystem
	Entries(context.Context) ([]artifactEntry, error)
	Open(context.Context, string) (io.ReadCloser, error)
}

type programArtifact struct {
	Digest    string
	SizeBytes int64
	MediaType string
	Reader    artifactReader
}

type programArtifacts struct {
	Code         programArtifact
	Dependencies programArtifact
}

type verifiedProgram struct {
	index        ProgramIndex
	dependencies DependencyIndex
	graph        PackageGraph
	modules      ModuleMap
}

func (program *verifiedProgram) Index() ProgramIndex {
	index := program.index
	index.Declarations = append([]ProgramDeclaration(nil), index.Declarations...)
	for position := range index.Declarations {
		index.Declarations[position].Slots = append(
			[]DeclarationSlot(nil),
			index.Declarations[position].Slots...,
		)
	}
	return index
}

func (program *verifiedProgram) DependencyIndex() DependencyIndex {
	return program.dependencies
}

func verifyProgramArtifacts(ctx context.Context, artifacts programArtifacts) (*verifiedProgram, error) {
	if err := validateArtifactDescriptor(artifacts.Code, codeArtifact); err != nil {
		return nil, err
	}
	if err := validateArtifactDescriptor(artifacts.Dependencies, dependencyArtifact); err != nil {
		return nil, err
	}

	code, err := inspectArtifact(ctx, artifacts.Code.Reader, codeArtifact, maxCodeLogicalBytes)
	if err != nil {
		return nil, fmt.Errorf("code Artifact: %w", err)
	}
	dependencies, err := inspectArtifact(
		ctx,
		artifacts.Dependencies.Reader,
		dependencyArtifact,
		maxDependencyLogicalBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("dependency Artifact: %w", err)
	}

	verifier := pairVerifier{
		ctx:       ctx,
		artifacts: artifacts,
		code:      code,
		deps:      dependencies,
	}
	if err := verifier.verify(); err != nil {
		return nil, err
	}
	return &verifiedProgram{
		index:        verifier.index,
		dependencies: verifier.dependencyIndex,
		graph:        verifier.graph,
		modules:      verifier.modules,
	}, nil
}

type artifactRole uint8

const (
	codeArtifact artifactRole = iota
	dependencyArtifact
)

type inspectedArtifact struct {
	reader  artifactReader
	role    artifactRole
	entries map[string]artifactEntry
	ordered []artifactEntry
}

func validateArtifactDescriptor(
	artifact programArtifact,
	role artifactRole,
) error {
	var label, mediaType string
	var maxPhysicalBytes int64
	switch role {
	case codeArtifact:
		label = "code"
		mediaType = ProgramCodeArtifactMediaType
		maxPhysicalBytes = maxCodePhysicalBytes
	case dependencyArtifact:
		label = "dependency"
		mediaType = ProgramDependencyArtifactMediaType
		maxPhysicalBytes = maxDependencyPhysicalBytes
	default:
		return fmt.Errorf("Artifact role = %d", role)
	}
	if !sha256DigestPattern.MatchString(artifact.Digest) {
		return fmt.Errorf("%s Artifact digest is not a lowercase SHA-256 digest", label)
	}
	if artifact.SizeBytes < 1 || artifact.SizeBytes > maxPhysicalBytes {
		return fmt.Errorf(
			"%s Artifact size is outside [1,%d]",
			label,
			maxPhysicalBytes,
		)
	}
	if artifact.MediaType != mediaType {
		return fmt.Errorf("%s Artifact media type = %q, want %q", label, artifact.MediaType, mediaType)
	}
	if artifact.Reader == nil {
		return fmt.Errorf("%s Artifact reader is nil", label)
	}
	return nil
}

func inspectArtifact(
	ctx context.Context,
	reader artifactReader,
	role artifactRole,
	maxLogicalBytes int64,
) (*inspectedArtifact, error) {
	filesystem := reader.Filesystem()
	if filesystem.Major != 4 || filesystem.Minor != 0 ||
		filesystem.Compression != "zstd" || filesystem.BlockSize != 131072 ||
		filesystem.CreatedAtUnix != 0 || filesystem.HasFragments ||
		filesystem.HasDuplicatePacking || filesystem.HasOverlappingData ||
		filesystem.HasExportTable || filesystem.HasXattrs {
		return nil, fmt.Errorf("filesystem facts are outside the exact SquashFS v0 contract")
	}

	entries, err := reader.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate filesystem: %w", err)
	}
	if len(entries) == 0 || len(entries) > maxArtifactEntries {
		return nil, fmt.Errorf("entry count is outside [1,%d]", maxArtifactEntries)
	}

	inspected := &inspectedArtifact{
		reader:  reader,
		role:    role,
		entries: make(map[string]artifactEntry, len(entries)),
		ordered: make([]artifactEntry, 0, len(entries)),
	}
	inodes := make(map[uint64]string)
	var logicalBytes int64
	var nameBytes int64
	for position, entry := range entries {
		nameBytes, err = chargeArtifactNameBytes(nameBytes, entry)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", position, err)
		}
		if err := validateArtifactEntry(entry, role); err != nil {
			return nil, fmt.Errorf("entry %d %q: %w", position, entry.Path, err)
		}
		if _, exists := inspected.entries[entry.Path]; exists {
			return nil, fmt.Errorf("duplicate path %q", entry.Path)
		}
		if entry.Kind != artifactEntryDirectory {
			if previous, exists := inodes[entry.Inode]; exists {
				return nil, fmt.Errorf("paths %q and %q share inode %d", previous, entry.Path, entry.Inode)
			}
			inodes[entry.Inode] = entry.Path
		}
		if entry.Kind == artifactEntryRegular {
			if logicalBytes > maxLogicalBytes-entry.SizeBytes {
				return nil, fmt.Errorf("aggregate logical regular-file bytes exceed %d", maxLogicalBytes)
			}
			logicalBytes += entry.SizeBytes
		}
		inspected.entries[entry.Path] = entry
		inspected.ordered = append(inspected.ordered, entry)
	}
	root, exists := inspected.entries["."]
	if !exists || root.Kind != artifactEntryDirectory {
		return nil, fmt.Errorf("filesystem root must be an enumerated directory")
	}
	for _, entry := range inspected.ordered {
		if entry.Path == "." {
			continue
		}
		parent := path.Dir(entry.Path)
		parentEntry, exists := inspected.entries[parent]
		if !exists || parentEntry.Kind != artifactEntryDirectory {
			return nil, fmt.Errorf("path %q has no enumerated directory parent %q", entry.Path, parent)
		}
	}
	return inspected, nil
}

func chargeArtifactNameBytes(total int64, entry artifactEntry) (int64, error) {
	if total < 0 {
		return 0, fmt.Errorf("aggregate raw path and symbolic-link-target bytes are negative")
	}
	for _, size := range []int{len(entry.Path), len(entry.LinkTarget)} {
		bytes := int64(size)
		if total > maxArtifactNameBytes-bytes {
			return 0, fmt.Errorf(
				"aggregate raw path and symbolic-link-target bytes exceed %d",
				maxArtifactNameBytes,
			)
		}
		total += bytes
	}
	return total, nil
}

func validateArtifactEntry(entry artifactEntry, role artifactRole) error {
	if entry.UID != 0 || entry.GID != 0 || entry.ModTimeUnix != 0 || entry.HasXattrs {
		return fmt.Errorf("ownership, timestamp, or xattr metadata is not normalized")
	}
	if entry.SizeBytes < 0 {
		return fmt.Errorf("logical size is negative")
	}
	switch entry.Kind {
	case artifactEntryRegular:
		if entry.Mode != 0644 && entry.Mode != 0755 {
			return fmt.Errorf("regular-file mode %#o is unsupported", entry.Mode)
		}
		if entry.SizeBytes > maxArtifactFileSize {
			return fmt.Errorf("regular file exceeds %d bytes", maxArtifactFileSize)
		}
		if entry.LinkTarget != "" || entry.LinkCount != 1 {
			return fmt.Errorf("regular-file link metadata is invalid")
		}
	case artifactEntryDirectory:
		if entry.Mode != 0755 || entry.LinkTarget != "" {
			return fmt.Errorf("directory metadata is invalid")
		}
	case artifactEntrySymlink:
		if entry.Mode != 0777 || entry.LinkCount != 1 {
			return fmt.Errorf("symbolic-link metadata is invalid")
		}
		if entry.SizeBytes != int64(len(entry.LinkTarget)) {
			return fmt.Errorf("symbolic-link logical size does not equal target length")
		}
		if err := validateSymlinkTarget(entry.LinkTarget); err != nil {
			return err
		}
	default:
		return fmt.Errorf("inode kind %q is unsupported", entry.Kind)
	}
	return validateArtifactPath(entry.Path, role)
}

func validateArtifactPath(value string, role artifactRole) error {
	if value == "." {
		return nil
	}
	if value == "" || !utf8.ValidString(value) || strings.HasPrefix(value, "/") ||
		strings.Contains(value, "\\") {
		return fmt.Errorf("path is not a confined relative POSIX path")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("path contains a control character")
		}
	}
	components := strings.Split(value, "/")
	if len(components) > maxArtifactDepth {
		return fmt.Errorf("path depth exceeds %d", maxArtifactDepth)
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." || len(component) > maxPackagePathComponent {
			return fmt.Errorf("path is not normalized or exceeds a component bound")
		}
	}
	mount := programMountPath
	if role == dependencyArtifact {
		mount = dependencyMountPath
	}
	if len(mount)+1+len(value)+1 > maxMountedPackagePath {
		return fmt.Errorf("mounted path exceeds %d bytes", maxMountedPackagePath)
	}
	return nil
}

func validateSymlinkTarget(target string) error {
	if target == "" || len(target) > maxSymlinkTargetBytes || !utf8.ValidString(target) ||
		strings.HasPrefix(target, "/") || strings.Contains(target, "\\") {
		return fmt.Errorf("symbolic-link target is not an admitted relative POSIX path")
	}
	for _, character := range target {
		if unicode.IsControl(character) {
			return fmt.Errorf("symbolic-link target contains a control character")
		}
	}
	for _, component := range strings.Split(target, "/") {
		if component == "" || len(component) > maxPackagePathComponent {
			return fmt.Errorf("symbolic-link target has an empty or oversized component")
		}
	}
	return nil
}

func (artifact *inspectedArtifact) require(path string, kind artifactEntryKind) (artifactEntry, error) {
	entry, exists := artifact.entries[path]
	if !exists {
		return artifactEntry{}, fmt.Errorf("required path %q is missing", path)
	}
	if entry.Kind != kind {
		return artifactEntry{}, fmt.Errorf("path %q kind = %q, want %q", path, entry.Kind, kind)
	}
	return entry, nil
}

func (artifact *inspectedArtifact) read(
	ctx context.Context,
	path string,
	maxBytes int64,
) ([]byte, error) {
	entry, err := artifact.require(path, artifactEntryRegular)
	if err != nil {
		return nil, err
	}
	if entry.SizeBytes > maxBytes {
		return nil, fmt.Errorf("path %q exceeds %d bytes", path, maxBytes)
	}
	reader, err := artifact.reader.Open(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(raw)) != entry.SizeBytes {
		return nil, fmt.Errorf("path %q read %d bytes, metadata declares %d", path, len(raw), entry.SizeBytes)
	}
	return raw, nil
}

func (artifact *inspectedArtifact) digest(ctx context.Context, path string) (string, error) {
	entry, err := artifact.require(path, artifactEntryRegular)
	if err != nil {
		return "", err
	}
	reader, err := artifact.reader.Open(ctx, path)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", path, err)
	}
	defer reader.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(reader, entry.SizeBytes+1))
	if err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	if written != entry.SizeBytes {
		return "", fmt.Errorf("path %q read %d bytes, metadata declares %d", path, written, entry.SizeBytes)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func digestBytes(raw []byte) string {
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func joinArtifactPath(parts ...string) string {
	joined := path.Join(parts...)
	if joined == "" {
		return "."
	}
	return joined
}

func pointerStringsEqual(left, right *string) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}
