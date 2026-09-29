package artifact

import (
	"context"
	"fmt"
	"io"
	"math"
	"path"

	"github.com/helmrdotdev/helmr/internal/safepath"
)

const (
	MaxEntries                      = 200000
	MaxFileSize               int64 = 1 << 30
	MaxNameBytes              int64 = 128 << 20
	MaxProgramLogicalBytes    int64 = 10 << 30
	MaxBuildTreeLogicalBytes        = MaxProgramLogicalBytes
	MaxProgramPhysicalBytes   int64 = 13 << 30
	MaxBuildTreePhysicalBytes       = MaxProgramPhysicalBytes
	MaxRuntimeLogicalBytes    int64 = 2 << 30
	MaxRuntimePhysicalBytes   int64 = 3 << 30
	maxPackageJSONBytes       int64 = 256 << 20
	maxLockfileBytes          int64 = 64 << 20
	ProgramMountPath                = "/opt/helmr/program"
)

// MaxProgramTreeEntries bounds producer-side tree materialization before the
// same entries are inspected through the Program artifact verifier.
const MaxProgramTreeEntries = MaxEntries

type EntryKind string

const (
	EntryRegular         = EntryKind("regular")
	EntryDirectory       = EntryKind("directory")
	EntrySymlink         = EntryKind("symlink")
	EntryBlockDevice     = EntryKind("block-device")
	EntryCharacterDevice = EntryKind("character-device")
	EntryFIFO            = EntryKind("fifo")
	EntrySocket          = EntryKind("socket")
)

type Entry struct {
	Path        string
	Kind        EntryKind
	Form        uint16
	Mode        uint32
	SizeBytes   int64
	UIDIndex    uint16
	GIDIndex    uint16
	UID         uint32
	GID         uint32
	ModTimeUnix uint32
	XattrIndex  uint32
	LinkTarget  string
	Inode       uint64
	InodeNumber uint32
	LinkCount   uint32
}

type Role uint8

const (
	RoleProgram Role = iota
	RoleRuntime
	RoleBuildTree
)

// opener opens regular files by their artifact-relative path.
type opener interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

// Tree is an enumerated artifact filesystem whose entries have
// passed inspection for one role.
type Tree struct {
	reader  opener
	role    Role
	entries map[string]Entry
	ordered []Entry
}

// newTree records entries that have already passed inspection in
// their enumeration order.
func newTree(
	reader opener,
	role Role,
	ordered []Entry,
) *Tree {
	tree := &Tree{
		reader:  reader,
		role:    role,
		entries: make(map[string]Entry, len(ordered)),
		ordered: ordered,
	}
	for _, entry := range ordered {
		tree.entries[entry.Path] = entry
	}
	return tree
}

// Role reports the artifact role the entries were inspected for.
func (tree *Tree) Role() Role {
	return tree.role
}

// Entries returns the inspected entries in enumeration order. Callers must
// not modify the returned slice.
func (tree *Tree) Entries() []Entry {
	return tree.ordered
}

// Lookup returns the inspected entry at path.
func (tree *Tree) Lookup(path string) (Entry, bool) {
	entry, exists := tree.entries[path]
	return entry, exists
}

// Open opens the regular file at path.
func (tree *Tree) Open(ctx context.Context, path string) (io.ReadCloser, error) {
	return tree.reader.Open(ctx, path)
}

func chargeNameBytes(total int64, entry Entry) (int64, error) {
	if total < 0 {
		return 0, fmt.Errorf("aggregate raw path and symbolic-link-target bytes are negative")
	}
	for _, size := range []int{len(entry.Path), len(entry.LinkTarget)} {
		bytes := int64(size)
		if total > MaxNameBytes-bytes {
			return 0, fmt.Errorf(
				"aggregate raw path and symbolic-link-target bytes exceed %d",
				MaxNameBytes,
			)
		}
		total += bytes
	}
	return total, nil
}

func ValidatePath(value string, role Role) error {
	mount := ProgramMountPath
	switch role {
	case RoleRuntime:
		mount = RuntimeMountPath
	}
	return safepath.ValidateTreePath(value, mount)
}

func ValidateLinkTarget(target string) error {
	return safepath.ValidateTreeLink(target)
}

func (tree *Tree) Require(path string, kind EntryKind) (Entry, error) {
	entry, exists := tree.entries[path]
	if !exists {
		return Entry{}, fmt.Errorf("required path %q is missing", path)
	}
	if entry.Kind != kind {
		return Entry{}, fmt.Errorf("path %q kind = %q, want %q", path, entry.Kind, kind)
	}
	return entry, nil
}

func (tree *Tree) Read(
	ctx context.Context,
	path string,
	maxBytes int64,
) ([]byte, error) {
	entry, err := tree.Require(path, EntryRegular)
	if err != nil {
		return nil, err
	}
	if entry.SizeBytes > maxBytes {
		return nil, fmt.Errorf("path %q exceeds %d bytes", path, maxBytes)
	}
	reader, err := tree.reader.Open(ctx, path)
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

type Filesystem struct {
	Magic               uint32
	InodeCount          uint32
	CreatedAtUnix       uint32
	BlockSize           uint32
	FragmentCount       uint32
	Compressor          uint16
	BlockLog            uint16
	Flags               uint16
	IDCount             uint16
	Major               uint16
	Minor               uint16
	RootInodeReference  uint64
	BytesUsed           uint64
	PhysicalSize        uint64
	IDTableStart        uint64
	XattrIDTableStart   uint64
	InodeTableStart     uint64
	DirectoryTableStart uint64
	FragmentTableStart  uint64
	ExportTableStart    uint64
	IDs                 []uint32
	HasZeroPadding      bool
	HasFragmentRefs     bool
	HasOverlappingData  bool
}

type Reader interface {
	Filesystem() Filesystem
	Entries(context.Context) ([]Entry, error)
	Open(context.Context, string) (io.ReadCloser, error)
}

func Inspect(
	ctx context.Context,
	reader Reader,
	role Role,
	maxLogicalBytes int64,
	physicalSize int64,
) (*Tree, error) {
	filesystem := reader.Filesystem()
	if err := ValidateFilesystem(filesystem, physicalSize, false); err != nil {
		return nil, err
	}

	entries, err := reader.Entries(ctx)
	if err != nil {
		return nil, fmt.Errorf("enumerate filesystem: %w", err)
	}
	if len(entries) == 0 || len(entries) > MaxEntries {
		return nil, fmt.Errorf("entry count is outside [1,%d]", MaxEntries)
	}
	filesystem = reader.Filesystem()
	if err := ValidateFilesystem(filesystem, physicalSize, true); err != nil {
		return nil, err
	}

	byPath := make(map[string]Entry, len(entries))
	ordered := make([]Entry, 0, len(entries))
	inodes := make(map[uint64]string)
	inodeNumbers := make(map[uint32]string)
	var logicalBytes int64
	var nameBytes int64
	for position, entry := range entries {
		nameBytes, err = chargeNameBytes(nameBytes, entry)
		if err != nil {
			return nil, fmt.Errorf("entry %d: %w", position, err)
		}
		if err := ValidateEntry(entry, role); err != nil {
			return nil, fmt.Errorf("entry %d %q: %w", position, entry.Path, err)
		}
		if entry.InodeNumber > filesystem.InodeCount {
			return nil, fmt.Errorf(
				"entry %d %q inode number %d exceeds superblock count %d",
				position,
				entry.Path,
				entry.InodeNumber,
				filesystem.InodeCount,
			)
		}
		if _, exists := byPath[entry.Path]; exists {
			return nil, fmt.Errorf("duplicate path %q", entry.Path)
		}
		if previous, exists := inodes[entry.Inode]; exists {
			return nil, fmt.Errorf("paths %q and %q share inode reference %#x", previous, entry.Path, entry.Inode)
		}
		inodes[entry.Inode] = entry.Path
		if previous, exists := inodeNumbers[entry.InodeNumber]; exists {
			return nil, fmt.Errorf(
				"paths %q and %q share inode number %d",
				previous,
				entry.Path,
				entry.InodeNumber,
			)
		}
		inodeNumbers[entry.InodeNumber] = entry.Path
		if entry.Kind == EntryRegular {
			if logicalBytes > maxLogicalBytes-entry.SizeBytes {
				return nil, fmt.Errorf("aggregate logical regular-file bytes exceed %d", maxLogicalBytes)
			}
			logicalBytes += entry.SizeBytes
		}
		byPath[entry.Path] = entry
		ordered = append(ordered, entry)
	}
	root, exists := byPath["."]
	if !exists || root.Kind != EntryDirectory {
		return nil, fmt.Errorf("filesystem root must be an enumerated directory")
	}
	if root.Inode != filesystem.RootInodeReference {
		return nil, fmt.Errorf(
			"filesystem root inode reference = %#x, enumerated root = %#x",
			filesystem.RootInodeReference,
			root.Inode,
		)
	}
	if uint64(len(inodes)) != uint64(filesystem.InodeCount) {
		return nil, fmt.Errorf(
			"enumerated unique inode count = %d, superblock declares %d",
			len(inodes),
			filesystem.InodeCount,
		)
	}
	for _, entry := range ordered {
		if entry.Path == "." {
			continue
		}
		parent := path.Dir(entry.Path)
		parentEntry, exists := byPath[parent]
		if !exists || parentEntry.Kind != EntryDirectory {
			return nil, fmt.Errorf("path %q has no enumerated directory parent %q", entry.Path, parent)
		}
	}
	return newTree(reader, role, ordered), nil
}

func ValidateFilesystem(
	filesystem Filesystem,
	physicalSize int64,
	complete bool,
) error {
	if filesystem.Magic != SquashFSMagic ||
		filesystem.Major != 4 ||
		filesystem.Minor != 0 ||
		filesystem.Compressor != SquashFSZstandardCompressor ||
		filesystem.BlockSize != SquashFSDataBlockSize ||
		filesystem.BlockLog != 17 ||
		filesystem.CreatedAtUnix != 0 ||
		filesystem.Flags != SquashFSV0Flags ||
		filesystem.FragmentCount != 0 ||
		filesystem.IDCount != 1 ||
		filesystem.XattrIDTableStart != math.MaxUint64 ||
		filesystem.ExportTableStart != math.MaxUint64 {
		return fmt.Errorf("filesystem facts are outside the exact SquashFS v0 contract")
	}
	if physicalSize < 0 || filesystem.PhysicalSize != uint64(physicalSize) {
		return fmt.Errorf(
			"filesystem physical size = %d, descriptor declares %d",
			filesystem.PhysicalSize,
			physicalSize,
		)
	}
	expected, ok := RoundUpSquashFSSize(filesystem.BytesUsed, SquashFSPhysicalAlign)
	if !ok || filesystem.PhysicalSize != expected || !filesystem.HasZeroPadding {
		return fmt.Errorf("filesystem tail is outside the exact SquashFS v0 contract")
	}
	if !complete {
		return nil
	}
	if len(filesystem.IDs) != 1 || filesystem.IDs[0] != 0 ||
		filesystem.HasFragmentRefs || filesystem.HasOverlappingData {
		return fmt.Errorf("filesystem contents are outside the exact SquashFS v0 contract")
	}
	return nil
}

func ValidateEntry(entry Entry, role Role) error {
	if entry.UIDIndex != 0 || entry.GIDIndex != 0 ||
		entry.UID != 0 || entry.GID != 0 ||
		entry.ModTimeUnix != 0 || entry.XattrIndex != SquashFSInvalidXattr {
		return fmt.Errorf("ownership, timestamp, or xattr metadata is not normalized")
	}
	if entry.InodeNumber == 0 {
		return fmt.Errorf("inode number is zero")
	}
	if entry.SizeBytes < 0 {
		return fmt.Errorf("logical size is negative")
	}
	switch entry.Kind {
	case EntryRegular:
		if entry.Form != SquashFSBasicRegularForm &&
			entry.Form != SquashFSExtendedRegularForm {
			return fmt.Errorf("regular-file inode form %d is unsupported", entry.Form)
		}
		if entry.Mode != 0644 && entry.Mode != 0755 {
			return fmt.Errorf("regular-file mode %#o is unsupported", entry.Mode)
		}
		if entry.SizeBytes > MaxFileSize {
			return fmt.Errorf("regular file exceeds %d bytes", MaxFileSize)
		}
		if entry.LinkTarget != "" || entry.LinkCount != 1 {
			return fmt.Errorf("regular-file link metadata is invalid")
		}
	case EntryDirectory:
		if entry.Form != SquashFSBasicDirectoryForm &&
			entry.Form != SquashFSExtendedDirectoryForm {
			return fmt.Errorf("directory inode form %d is unsupported", entry.Form)
		}
		if entry.Mode != 0755 || entry.LinkTarget != "" {
			return fmt.Errorf("directory metadata is invalid")
		}
	case EntrySymlink:
		if entry.Form != SquashFSBasicSymlinkForm {
			return fmt.Errorf("symbolic-link inode form %d is unsupported", entry.Form)
		}
		if entry.Mode != 0777 || entry.LinkCount != 1 {
			return fmt.Errorf("symbolic-link metadata is invalid")
		}
		if entry.SizeBytes != int64(len(entry.LinkTarget)) {
			return fmt.Errorf("symbolic-link logical size does not equal target length")
		}
		if err := ValidateLinkTarget(entry.LinkTarget); err != nil {
			return err
		}
	default:
		return fmt.Errorf("inode kind %q is unsupported", entry.Kind)
	}
	return ValidatePath(entry.Path, role)
}
