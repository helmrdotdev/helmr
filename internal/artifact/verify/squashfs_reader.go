package verify

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

type squashFSArtifactReader struct {
	source io.ReaderAt
	role   artifact.Role

	once sync.Once
	mu   sync.RWMutex

	filesystem artifact.Filesystem
	entries    []artifact.Entry
	data       *squashFSDataReader
	err        error
}

// NewSquashFSReader decodes the SquashFS image in source so it can be
// inspected as an artifact of role.
func NewSquashFSReader(
	ctx context.Context,
	source io.ReaderAt,
	physicalSize int64,
	role artifact.Role,
) (artifact.Reader, error) {
	reader, err := newSquashFSArtifactReader(ctx, source, physicalSize, role)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func newSquashFSArtifactReader(
	ctx context.Context,
	source io.ReaderAt,
	physicalSize int64,
	role artifact.Role,
) (*squashFSArtifactReader, error) {
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	if _, err := artifactLogicalLimit(role); err != nil {
		return nil, err
	}
	physicalLimit, err := artifactPhysicalLimit(role)
	if err != nil {
		return nil, err
	}
	if physicalSize > physicalLimit {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS physical size = %d, want at most %d",
				physicalSize,
				physicalLimit,
			),
		}
	}
	facts, err := readSquashFSFacts(source, physicalSize)
	if err != nil {
		return nil, err
	}
	return &squashFSArtifactReader{
		source:     source,
		role:       role,
		filesystem: projectSquashFSFilesystem(facts),
	}, nil
}

func (reader *squashFSArtifactReader) Filesystem() artifact.Filesystem {
	if reader == nil {
		return artifact.Filesystem{}
	}
	reader.mu.RLock()
	defer reader.mu.RUnlock()
	return cloneArtifactFilesystem(reader.filesystem)
}

func (reader *squashFSArtifactReader) Entries(
	ctx context.Context,
) ([]artifact.Entry, error) {
	if reader == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS artifact reader is nil"),
		}
	}
	reader.once.Do(func() {
		err := reader.readEntries(ctx)
		reader.mu.Lock()
		reader.err = err
		reader.mu.Unlock()
	})
	reader.mu.RLock()
	defer reader.mu.RUnlock()
	if reader.err != nil {
		return nil, reader.err
	}
	return append([]artifact.Entry(nil), reader.entries...), nil
}

func (reader *squashFSArtifactReader) Open(
	ctx context.Context,
	path string,
) (io.ReadCloser, error) {
	if reader == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS artifact reader is nil"),
		}
	}
	reader.mu.RLock()
	data := reader.data
	err := reader.err
	reader.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if data == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS artifact entries have not been read"),
		}
	}
	return data.Open(ctx, path)
}

func (reader *squashFSArtifactReader) readEntries(ctx context.Context) error {
	if err := checkSquashFSContext(ctx); err != nil {
		return err
	}
	limit, err := artifactLogicalLimit(reader.role)
	if err != nil {
		return err
	}

	reader.mu.RLock()
	superblock := projectArtifactSuperblock(reader.filesystem)
	reader.mu.RUnlock()

	decoder, err := newSquashFSMetadataDecoder(reader.source)
	if err != nil {
		return err
	}
	defer decoder.Close()

	ids, err := readSquashFSIDTable(
		ctx,
		decoder,
		superblock.FragmentTableStart,
		superblock.IDTableStart,
		superblock.BytesUsed,
		superblock.IDCount,
	)
	if err != nil {
		return fmt.Errorf("read SquashFS ID table: %w", err)
	}
	tree, err := readSquashFSTree(
		ctx,
		decoder,
		superblock,
		ids,
		uint64(limit),
	)
	if err != nil {
		return fmt.Errorf("read SquashFS tree: %w", err)
	}
	entries, err := projectSquashFSEntries(tree.Paths)
	if err != nil {
		return err
	}
	data, err := newSquashFSDataReader(ctx, reader.source, superblock, tree)
	if err != nil {
		return fmt.Errorf("read SquashFS file data: %w", err)
	}

	reader.mu.Lock()
	reader.filesystem.IDs = append([]uint32(nil), ids...)
	reader.filesystem.HasFragmentRefs = tree.HasFragmentReferences
	reader.filesystem.HasOverlappingData = tree.HasOverlappingData
	reader.entries = entries
	reader.data = data
	reader.mu.Unlock()
	return nil
}

func artifactLogicalLimit(role artifact.Role) (int64, error) {
	switch role {
	case artifact.RoleProgram:
		return artifact.MaxProgramLogicalBytes, nil
	case artifact.RoleRuntime:
		return artifact.MaxRuntimeLogicalBytes, nil
	case artifact.RoleBuildTree:
		return artifact.MaxBuildTreeLogicalBytes, nil
	default:
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("artifact role = %d", role),
		}
	}
}

func artifactPhysicalLimit(role artifact.Role) (int64, error) {
	switch role {
	case artifact.RoleProgram:
		return artifact.MaxProgramPhysicalBytes, nil
	case artifact.RoleRuntime:
		return artifact.MaxRuntimePhysicalBytes, nil
	case artifact.RoleBuildTree:
		return artifact.MaxBuildTreePhysicalBytes, nil
	default:
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("artifact role = %d", role),
		}
	}
}

func projectSquashFSFilesystem(facts squashFSFacts) artifact.Filesystem {
	superblock := facts.Superblock
	return artifact.Filesystem{
		Magic:               superblock.Magic,
		InodeCount:          superblock.InodeCount,
		CreatedAtUnix:       superblock.CreatedAtUnix,
		BlockSize:           superblock.BlockSize,
		FragmentCount:       superblock.FragmentCount,
		Compressor:          superblock.Compressor,
		BlockLog:            superblock.BlockLog,
		Flags:               superblock.Flags,
		IDCount:             superblock.IDCount,
		Major:               superblock.Major,
		Minor:               superblock.Minor,
		RootInodeReference:  superblock.RootInodeReference,
		BytesUsed:           superblock.BytesUsed,
		PhysicalSize:        facts.PhysicalSize,
		IDTableStart:        superblock.IDTableStart,
		XattrIDTableStart:   superblock.XattrIDTableStart,
		InodeTableStart:     superblock.InodeTableStart,
		DirectoryTableStart: superblock.DirectoryTableStart,
		FragmentTableStart:  superblock.FragmentTableStart,
		ExportTableStart:    superblock.ExportTableStart,
		HasZeroPadding:      facts.Tail.HasZeroPadding,
	}
}

func projectArtifactSuperblock(filesystem artifact.Filesystem) squashFSSuperblockFacts {
	return squashFSSuperblockFacts{
		Magic:               filesystem.Magic,
		InodeCount:          filesystem.InodeCount,
		CreatedAtUnix:       filesystem.CreatedAtUnix,
		BlockSize:           filesystem.BlockSize,
		FragmentCount:       filesystem.FragmentCount,
		Compressor:          filesystem.Compressor,
		BlockLog:            filesystem.BlockLog,
		Flags:               filesystem.Flags,
		IDCount:             filesystem.IDCount,
		Major:               filesystem.Major,
		Minor:               filesystem.Minor,
		RootInodeReference:  filesystem.RootInodeReference,
		BytesUsed:           filesystem.BytesUsed,
		IDTableStart:        filesystem.IDTableStart,
		XattrIDTableStart:   filesystem.XattrIDTableStart,
		InodeTableStart:     filesystem.InodeTableStart,
		DirectoryTableStart: filesystem.DirectoryTableStart,
		FragmentTableStart:  filesystem.FragmentTableStart,
		ExportTableStart:    filesystem.ExportTableStart,
	}
}

func projectSquashFSEntries(paths []squashFSPathFacts) ([]artifact.Entry, error) {
	entries := make([]artifact.Entry, 0, len(paths))
	for _, path := range paths {
		if path.Inode == nil {
			return nil, &artifactInfrastructureError{
				cause: fmt.Errorf("SquashFS path %q has no inode", path.Path),
			}
		}
		inode := path.Inode
		kind, err := projectSquashFSKind(inode.Kind)
		if err != nil {
			return nil, err
		}
		size := int64(0)
		switch inode.Kind {
		case squashFSRegularKind:
			if inode.Regular == nil {
				return nil, &artifactInfrastructureError{
					cause: fmt.Errorf("SquashFS regular path %q has no facts", path.Path),
				}
			}
			size = int64(inode.Regular.Size)
		case squashFSSymlinkKind:
			size = int64(len(inode.LinkTarget))
		}
		entries = append(entries, artifact.Entry{
			Path:        path.Path,
			Kind:        kind,
			Form:        inode.Form,
			Mode:        uint32(inode.Mode),
			SizeBytes:   size,
			UIDIndex:    inode.UIDIndex,
			GIDIndex:    inode.GIDIndex,
			UID:         inode.UID,
			GID:         inode.GID,
			ModTimeUnix: inode.ModTimeUnix,
			XattrIndex:  inode.XattrIndex,
			LinkTarget:  inode.LinkTarget,
			Inode:       inode.Reference,
			InodeNumber: inode.InodeNumber,
			LinkCount:   inode.LinkCount,
		})
	}
	return entries, nil
}

func projectSquashFSKind(kind squashFSInodeKind) (artifact.EntryKind, error) {
	switch kind {
	case squashFSDirectoryKind:
		return artifact.EntryDirectory, nil
	case squashFSRegularKind:
		return artifact.EntryRegular, nil
	case squashFSSymlinkKind:
		return artifact.EntrySymlink, nil
	case squashFSBlockDeviceKind:
		return artifact.EntryBlockDevice, nil
	case squashFSCharDeviceKind:
		return artifact.EntryCharacterDevice, nil
	case squashFSFIFODeviceKind:
		return artifact.EntryFIFO, nil
	case squashFSSocketKind:
		return artifact.EntrySocket, nil
	default:
		return "", &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS inode kind = %d", kind),
		}
	}
}

func cloneArtifactFilesystem(filesystem artifact.Filesystem) artifact.Filesystem {
	filesystem.IDs = append([]uint32(nil), filesystem.IDs...)
	return filesystem
}
