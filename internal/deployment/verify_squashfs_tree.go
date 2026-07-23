package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type squashFSDirectoryEntryFacts struct {
	ParentReference   uint64
	Name              string
	DeclaredForm      uint16
	HeaderInodeNumber uint32
	InodeDelta        int16
	InodeNumber       uint32
	Reference         uint64
	ActualForm        uint16
	ActualInodeNumber uint32
}

type squashFSPathFacts struct {
	Path            string
	Depth           uint16
	ParentReference uint64
	Inode           *squashFSInodeFacts
	Edge            squashFSDirectoryEntryFacts
	HasEdge         bool
}

type squashFSTreeFacts struct {
	RootReference         uint64
	Paths                 []squashFSPathFacts
	Edges                 []squashFSDirectoryEntryFacts
	Inodes                map[uint64]*squashFSInodeFacts
	DataExtents           []squashFSBlockFacts
	LogicalBytes          uint64
	RetainedRawBytes      uint64
	DirectoryIndexCount   uint64
	BlockDescriptorCount  uint64
	HasFragmentReferences bool
	HasOverlappingData    bool
}

type squashFSTreeReader struct {
	ctx             context.Context
	metadata        *squashFSMetadataDecoder
	superblock      squashFSSuperblockFacts
	inode           *squashFSInodeDecoder
	directories     *squashFSMetadataCache
	inodes          map[uint64]*squashFSInodeFacts
	numbers         map[uint32]uint64
	extents         []squashFSBlockFacts
	directoryRanges []squashFSMetadataRange
	entryCount      uint64
}

func newSquashFSTreeReader(
	ctx context.Context,
	decoder *squashFSMetadataDecoder,
	superblock squashFSSuperblockFacts,
	ids []uint32,
	maxLogicalBytes uint64,
) (*squashFSTreeReader, error) {
	inode, err := newSquashFSInodeDecoder(
		ctx,
		decoder,
		superblock,
		ids,
		maxLogicalBytes,
	)
	if err != nil {
		return nil, err
	}
	return &squashFSTreeReader{
		ctx:        ctx,
		metadata:   decoder,
		superblock: superblock,
		inode:      inode,
		inodes:     make(map[uint64]*squashFSInodeFacts),
		numbers:    make(map[uint32]uint64),
		entryCount: 1,
	}, nil
}

func readSquashFSTree(
	ctx context.Context,
	decoder *squashFSMetadataDecoder,
	superblock squashFSSuperblockFacts,
	ids []uint32,
	maxLogicalBytes uint64,
) (squashFSTreeFacts, error) {
	reader, err := newSquashFSTreeReader(
		ctx,
		decoder,
		superblock,
		ids,
		maxLogicalBytes,
	)
	if err != nil {
		return squashFSTreeFacts{}, err
	}
	return reader.read()
}

func (reader *squashFSTreeReader) read() (squashFSTreeFacts, error) {
	if reader == nil || reader.inode == nil {
		return squashFSTreeFacts{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS tree reader is not initialized"),
		}
	}
	if err := checkSquashFSContext(reader.ctx); err != nil {
		return squashFSTreeFacts{}, err
	}
	if reader.superblock.RootInodeReference>>48 != 0 {
		return squashFSTreeFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS root inode reference %#x is not canonical",
				reader.superblock.RootInodeReference,
			),
		}
	}
	if reader.inode.retainedRawBytes > uint64(maxArtifactNameBytes)-1 {
		return squashFSTreeFacts{}, &artifactContentError{
			cause: fmt.Errorf("SquashFS retained raw bytes exceed %d", maxArtifactNameBytes),
		}
	}
	reader.inode.retainedRawBytes++

	root, err := reader.inodeAt(reader.superblock.RootInodeReference)
	if err != nil {
		return squashFSTreeFacts{}, fmt.Errorf("read SquashFS root inode: %w", err)
	}
	type pendingPath struct {
		path            string
		depth           uint16
		parentReference uint64
		inode           *squashFSInodeFacts
		edge            squashFSDirectoryEntryFacts
		hasEdge         bool
	}
	stack := []pendingPath{{
		path:  ".",
		inode: root,
	}}
	expandedDirectories := make(map[uint64]struct{})
	reachedDirectories := map[uint64]struct{}{
		root.Reference: {},
	}
	facts := squashFSTreeFacts{
		RootReference: reader.superblock.RootInodeReference,
		Paths:         make([]squashFSPathFacts, 0, min(reader.superblock.InodeCount, uint32(maxArtifactEntries))),
		Edges:         make([]squashFSDirectoryEntryFacts, 0),
		Inodes:        reader.inodes,
	}
	for len(stack) > 0 {
		if err := checkSquashFSContext(reader.ctx); err != nil {
			return squashFSTreeFacts{}, err
		}
		position := len(stack) - 1
		pending := stack[position]
		stack = stack[:position]
		facts.Paths = append(facts.Paths, squashFSPathFacts{
			Path:            pending.path,
			Depth:           pending.depth,
			ParentReference: pending.parentReference,
			Inode:           pending.inode,
			Edge:            pending.edge,
			HasEdge:         pending.hasEdge,
		})
		if pending.inode.Kind != squashFSDirectoryKind {
			continue
		}
		if _, ok := expandedDirectories[pending.inode.Reference]; ok {
			continue
		}
		expandedDirectories[pending.inode.Reference] = struct{}{}

		entries, err := reader.readDirectory(
			pending.inode,
			pending.path,
			pending.depth,
		)
		if err != nil {
			return squashFSTreeFacts{}, fmt.Errorf(
				"read SquashFS directory inode %d: %w",
				pending.inode.InodeNumber,
				err,
			)
		}
		children := make([]pendingPath, 0, len(entries))
		for _, entry := range entries {
			childPath := entry.Name
			if pending.path != "." {
				childPath = pending.path + "/" + entry.Name
			}
			child, err := reader.inodeAt(entry.Reference)
			if err != nil {
				return squashFSTreeFacts{}, fmt.Errorf(
					"read SquashFS path %q inode: %w",
					childPath,
					err,
				)
			}
			entry.ActualForm = child.Form
			entry.ActualInodeNumber = child.InodeNumber
			if entry.DeclaredForm != child.Form {
				return squashFSTreeFacts{}, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS path %q declares inode form %d, actual form %d",
						childPath,
						entry.DeclaredForm,
						child.Form,
					),
				}
			}
			if entry.InodeNumber != child.InodeNumber {
				return squashFSTreeFacts{}, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS path %q declares inode number %d, actual number %d",
						childPath,
						entry.InodeNumber,
						child.InodeNumber,
					),
				}
			}
			if child.Kind == squashFSDirectoryKind {
				if _, ok := reachedDirectories[entry.Reference]; ok {
					return squashFSTreeFacts{}, &artifactContentError{
						cause: fmt.Errorf(
							"SquashFS directory inode reference %#x is reachable more than once",
							entry.Reference,
						),
					}
				}
				reachedDirectories[entry.Reference] = struct{}{}
			}
			facts.Edges = append(facts.Edges, entry)
			children = append(children, pendingPath{
				path:            childPath,
				depth:           pending.depth + 1,
				parentReference: pending.inode.Reference,
				inode:           child,
				edge:            entry,
				hasEdge:         true,
			})
		}
		for position := len(children) - 1; position >= 0; position-- {
			stack = append(stack, children[position])
		}
	}
	if uint64(len(reader.inodes)) != uint64(reader.superblock.InodeCount) {
		return squashFSTreeFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS reachable inode count = %d, want %d",
				len(reader.inodes),
				reader.superblock.InodeCount,
			),
		}
	}
	if err := reader.inode.inodes.validateComplete(); err != nil {
		return squashFSTreeFacts{}, fmt.Errorf(
			"validate SquashFS inode metadata: %w",
			err,
		)
	}
	if err := validateSquashFSMetadataCoverage(
		reader.inode.ranges,
		reader.inode.inodes.bytes,
	); err != nil {
		return squashFSTreeFacts{}, fmt.Errorf(
			"validate SquashFS inode records: %w",
			err,
		)
	}
	if reader.directories != nil {
		if err := reader.directories.validateComplete(); err != nil {
			return squashFSTreeFacts{}, fmt.Errorf(
				"validate SquashFS directory metadata: %w",
				err,
			)
		}
		if err := validateSquashFSMetadataCoverage(
			reader.directoryRanges,
			reader.directories.bytes,
		); err != nil {
			return squashFSTreeFacts{}, fmt.Errorf(
				"validate SquashFS directory records: %w",
				err,
			)
		}
	} else if reader.superblock.DirectoryTableStart !=
		reader.superblock.FragmentTableStart {
		return squashFSTreeFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"unused SquashFS directory metadata range [%d, %d)",
				reader.superblock.DirectoryTableStart,
				reader.superblock.FragmentTableStart,
			),
		}
	}
	facts.DataExtents = append(facts.DataExtents, reader.extents...)
	facts.LogicalBytes = reader.inode.logicalBytes
	facts.RetainedRawBytes = reader.inode.retainedRawBytes
	facts.DirectoryIndexCount = reader.inode.indexCount
	facts.BlockDescriptorCount = reader.inode.blockCount
	for _, inode := range facts.Inodes {
		if inode.Regular != nil &&
			inode.Regular.Fragment != squashFSInvalidFragment {
			facts.HasFragmentReferences = true
			break
		}
	}
	facts.HasOverlappingData = squashFSDataExtentsOverlap(facts.DataExtents)
	return facts, nil
}

func squashFSDataExtentsOverlap(extents []squashFSBlockFacts) bool {
	ordered := append([]squashFSBlockFacts(nil), extents...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Start == ordered[right].Start {
			return ordered[left].End < ordered[right].End
		}
		return ordered[left].Start < ordered[right].Start
	})
	var previousEnd uint64
	for position, extent := range ordered {
		if position > 0 && extent.Start < previousEnd {
			return true
		}
		if extent.End > previousEnd {
			previousEnd = extent.End
		}
	}
	return false
}

func (reader *squashFSTreeReader) inodeAt(
	reference uint64,
) (*squashFSInodeFacts, error) {
	if inode, ok := reader.inodes[reference]; ok {
		return inode, nil
	}
	if len(reader.inodes) >= maxArtifactEntries {
		return nil, &artifactContentError{
			cause: fmt.Errorf("SquashFS inode count exceeds %d", maxArtifactEntries),
		}
	}
	inode, err := reader.inode.read(reference)
	if err != nil {
		return nil, err
	}
	if inode.InodeNumber == 0 ||
		inode.InodeNumber > reader.superblock.InodeCount {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS inode number %d is outside [1, %d]",
				inode.InodeNumber,
				reader.superblock.InodeCount,
			),
		}
	}
	if previous, ok := reader.numbers[inode.InodeNumber]; ok {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS inode number %d is shared by references %#x and %#x",
				inode.InodeNumber,
				previous,
				reference,
			),
		}
	}
	retained := inode
	reader.inodes[reference] = &retained
	reader.numbers[inode.InodeNumber] = reference
	if retained.Regular != nil {
		for _, block := range retained.Regular.Blocks {
			if !block.Sparse {
				reader.extents = append(reader.extents, block)
			}
		}
	}
	return &retained, nil
}

func (reader *squashFSTreeReader) readDirectory(
	inode *squashFSInodeFacts,
	parentPath string,
	parentDepth uint16,
) ([]squashFSDirectoryEntryFacts, error) {
	if reader == nil || inode == nil || inode.Directory == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS directory reader is not initialized"),
		}
	}
	if inode.Directory.EncodedSize < 3 {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS directory encoded size = %d, want at least 3",
				inode.Directory.EncodedSize,
			),
		}
	}
	remaining := uint64(inode.Directory.EncodedSize) - 3
	if remaining == 0 {
		return []squashFSDirectoryEntryFacts{}, nil
	}
	if reader.directories == nil {
		if reader.superblock.DirectoryTableStart >= reader.superblock.FragmentTableStart {
			return nil, &artifactContentError{
				cause: fmt.Errorf("SquashFS directory table is empty"),
			}
		}
		cache, err := newSquashFSMetadataCache(
			reader.ctx,
			reader.metadata,
			squashFSRegion{
				start: reader.superblock.DirectoryTableStart,
				end:   reader.superblock.FragmentTableStart,
			},
		)
		if err != nil {
			return nil, err
		}
		reader.directories = cache
	}
	physicalBlock, ok := addSquashFSUint64(
		reader.superblock.DirectoryTableStart,
		uint64(inode.Directory.StartBlock),
	)
	if !ok || physicalBlock >= reader.superblock.FragmentTableStart {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS directory block %d is outside the directory table",
				inode.Directory.StartBlock,
			),
		}
	}
	cursor, err := newSquashFSCachedCursor(
		reader.directories,
		physicalBlock,
		inode.Directory.Offset,
	)
	if err != nil {
		return nil, err
	}
	rangeStart, err := cursor.logicalPosition()
	if err != nil {
		return nil, err
	}
	limited := squashFSLimitedCursor{
		cursor:    cursor,
		remaining: remaining,
	}
	entries := make([]squashFSDirectoryEntryFacts, 0)
	order := binary.LittleEndian
	for limited.remaining > 0 {
		if err := checkSquashFSContext(reader.ctx); err != nil {
			return nil, err
		}
		var header [12]byte
		if err := limited.readFull(header[:]); err != nil {
			return nil, err
		}
		count := uint64(order.Uint32(header[0:4])) + 1
		if count > 256 {
			return nil, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS directory header count = %d, want at most 256",
					count,
				),
			}
		}
		if reader.entryCount > maxArtifactEntries-count {
			return nil, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS entry count exceeds %d",
					maxArtifactEntries,
				),
			}
		}
		reader.entryCount += count
		headerInode := order.Uint32(header[8:12])
		for position := uint64(0); position < count; position++ {
			var encoded [8]byte
			if err := limited.readFull(encoded[:]); err != nil {
				return nil, err
			}
			nameSize := uint64(order.Uint16(encoded[6:8])) + 1
			if nameSize > 256 {
				return nil, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS directory name size = %d, want at most 256",
						nameSize,
					),
				}
			}
			var name [256]byte
			if err := limited.readFull(name[:nameSize]); err != nil {
				return nil, err
			}
			component := string(name[:nameSize])
			if err := validateSquashFSComponent(component); err != nil {
				return nil, fmt.Errorf(
					"SquashFS directory entry name %q: %w",
					component,
					err,
				)
			}
			if len(entries) > 0 &&
				bytes.Compare(
					[]byte(entries[len(entries)-1].Name),
					[]byte(component),
				) >= 0 {
				return nil, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS directory names are not strictly increasing",
					),
				}
			}
			if parentDepth >= maxArtifactDepth {
				return nil, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS path depth exceeds %d",
						maxArtifactDepth,
					),
				}
			}
			childLength := nameSize
			if parentPath != "." {
				var ok bool
				childLength, ok = addSquashFSUint64(
					uint64(len(parentPath))+1,
					childLength,
				)
				if !ok {
					return nil, &artifactContentError{
						cause: fmt.Errorf("SquashFS path length overflows"),
					}
				}
			}
			if childLength+1 > maxMountedArtifactPathBytes ||
				reader.inode.retainedRawBytes > uint64(maxArtifactNameBytes)-childLength {
				return nil, &artifactContentError{
					cause: fmt.Errorf("SquashFS path exceeds retained-byte bounds"),
				}
			}
			reader.inode.retainedRawBytes += childLength

			inodeDelta := int16(order.Uint16(encoded[2:4]))
			delta := int64(inodeDelta)
			inodeNumber := int64(headerInode) + delta
			if inodeNumber < 0 || inodeNumber > math.MaxUint32 {
				return nil, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS directory inode delta %d from %d is out of range",
						delta,
						headerInode,
					),
				}
			}
			reference := uint64(order.Uint32(header[4:8]))<<16 |
				uint64(order.Uint16(encoded[0:2]))
			entries = append(entries, squashFSDirectoryEntryFacts{
				ParentReference:   inode.Reference,
				Name:              component,
				DeclaredForm:      order.Uint16(encoded[4:6]),
				HeaderInodeNumber: headerInode,
				InodeDelta:        inodeDelta,
				InodeNumber:       uint32(inodeNumber),
				Reference:         reference,
			})
		}
	}
	rangeEnd, err := cursor.logicalPosition()
	if err != nil {
		return nil, err
	}
	reader.directoryRanges = append(
		reader.directoryRanges,
		squashFSMetadataRange{start: rangeStart, end: rangeEnd},
	)
	return entries, nil
}

type squashFSLimitedCursor struct {
	cursor    *squashFSCachedCursor
	remaining uint64
}

func (cursor *squashFSLimitedCursor) readFull(destination []byte) error {
	if cursor == nil || cursor.cursor == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS limited cursor is not initialized"),
		}
	}
	if uint64(len(destination)) > cursor.remaining {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS directory record needs %d bytes with %d remaining",
				len(destination),
				cursor.remaining,
			),
		}
	}
	if err := cursor.cursor.readFull(destination); err != nil {
		return err
	}
	cursor.remaining -= uint64(len(destination))
	return nil
}

func validateSquashFSComponent(component string) error {
	if component == "" || component == "." || component == ".." ||
		len(component) > maxArtifactPathComponentBytes ||
		!utf8.ValidString(component) ||
		strings.ContainsAny(component, `/\`) {
		return &artifactContentError{
			cause: fmt.Errorf("component is not a confined UTF-8 POSIX name"),
		}
	}
	for _, character := range component {
		if unicode.IsControl(character) {
			return &artifactContentError{
				cause: fmt.Errorf("component contains a control character"),
			}
		}
	}
	return nil
}

func validateSquashFSMetadataCoverage(
	ranges []squashFSMetadataRange,
	size uint64,
) error {
	ordered := append([]squashFSMetadataRange(nil), ranges...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].start < ordered[right].start
	})
	var position uint64
	for _, current := range ordered {
		if current.start != position || current.end <= current.start ||
			current.end > size {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS metadata range [%d, %d) does not continue at %d within %d bytes",
					current.start,
					current.end,
					position,
					size,
				),
			}
		}
		position = current.end
	}
	if position != size {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata records cover %d of %d bytes",
				position,
				size,
			),
		}
	}
	return nil
}
