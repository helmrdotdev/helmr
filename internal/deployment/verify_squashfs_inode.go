package deployment

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
)

const (
	squashFSBasicDirectoryForm    = 1
	squashFSBasicRegularForm      = 2
	squashFSBasicSymlinkForm      = 3
	squashFSBasicBlockDeviceForm  = 4
	squashFSBasicCharDeviceForm   = 5
	squashFSBasicFIFOForm         = 6
	squashFSBasicSocketForm       = 7
	squashFSExtendedDirectoryForm = 8
	squashFSExtendedRegularForm   = 9
	squashFSExtendedSymlinkForm   = 10
	squashFSExtendedBlockForm     = 11
	squashFSExtendedCharForm      = 12
	squashFSExtendedFIFOForm      = 13
	squashFSExtendedSocketForm    = 14

	squashFSInvalidFragment      = math.MaxUint32
	squashFSInvalidXattr         = math.MaxUint32
	squashFSDataUncompressedBit  = uint32(1 << 24)
	squashFSDataBlockSize        = 131072
	squashFSMetadataCacheEntries = 64
	squashFSMetadataBlockLimit   = maxArtifactEntries
)

type squashFSInodeKind uint8

const (
	squashFSDirectoryKind squashFSInodeKind = iota + 1
	squashFSRegularKind
	squashFSSymlinkKind
	squashFSBlockDeviceKind
	squashFSCharDeviceKind
	squashFSFIFODeviceKind
	squashFSSocketKind
)

func squashFSInodeKindForForm(form uint16) (squashFSInodeKind, bool) {
	switch form {
	case squashFSBasicDirectoryForm, squashFSExtendedDirectoryForm:
		return squashFSDirectoryKind, true
	case squashFSBasicRegularForm, squashFSExtendedRegularForm:
		return squashFSRegularKind, true
	case squashFSBasicSymlinkForm, squashFSExtendedSymlinkForm:
		return squashFSSymlinkKind, true
	case squashFSBasicBlockDeviceForm, squashFSExtendedBlockForm:
		return squashFSBlockDeviceKind, true
	case squashFSBasicCharDeviceForm, squashFSExtendedCharForm:
		return squashFSCharDeviceKind, true
	case squashFSBasicFIFOForm, squashFSExtendedFIFOForm:
		return squashFSFIFODeviceKind, true
	case squashFSBasicSocketForm, squashFSExtendedSocketForm:
		return squashFSSocketKind, true
	default:
		return 0, false
	}
}

type squashFSInodeFacts struct {
	Reference   uint64
	Form        uint16
	Kind        squashFSInodeKind
	Mode        uint16
	UIDIndex    uint16
	GIDIndex    uint16
	UID         uint32
	GID         uint32
	ModTimeUnix uint32
	InodeNumber uint32
	LinkCount   uint32
	XattrIndex  uint32
	Device      uint32
	Directory   *squashFSDirectoryFacts
	Regular     *squashFSRegularFacts
	LinkTarget  string
}

type squashFSDirectoryFacts struct {
	StartBlock  uint32
	Offset      uint16
	EncodedSize uint32
	ParentInode uint32
	Indexes     []squashFSDirectoryIndexFacts
}

type squashFSDirectoryIndexFacts struct {
	Index      uint32
	StartBlock uint32
	Name       string
}

type squashFSRegularFacts struct {
	StartBlock  uint64
	Size        uint64
	SparseBytes uint64
	Fragment    uint32
	Offset      uint32
	Blocks      []squashFSBlockFacts
}

type squashFSBlockFacts struct {
	Encoded      uint32
	StoredSize   uint32
	LogicalSize  uint32
	Uncompressed bool
	Sparse       bool
	Start        uint64
	End          uint64
}

type squashFSMetadataRange struct {
	start uint64
	end   uint64
}

type squashFSMetadataCache struct {
	ctx      context.Context
	decoder  *squashFSMetadataDecoder
	region   squashFSRegion
	blocks   map[uint64]squashFSMetadataBlock
	order    []uint64
	starts   map[uint64]struct{}
	logical  map[uint64]uint64
	frontier uint64
	count    uint64
	bytes    uint64
}

func newSquashFSMetadataCache(
	ctx context.Context,
	decoder *squashFSMetadataDecoder,
	region squashFSRegion,
) (*squashFSMetadataCache, error) {
	if decoder == nil || decoder.reader == nil || decoder.decoder == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata decoder is not initialized"),
		}
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	if region.start >= region.end {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata region [%d, %d) is empty or reversed",
				region.start,
				region.end,
			),
		}
	}
	return &squashFSMetadataCache{
		ctx:      ctx,
		decoder:  decoder,
		region:   region,
		blocks:   make(map[uint64]squashFSMetadataBlock),
		order:    make([]uint64, 0, squashFSMetadataCacheEntries),
		starts:   make(map[uint64]struct{}),
		logical:  make(map[uint64]uint64),
		frontier: region.start,
	}, nil
}

func (cache *squashFSMetadataCache) block(
	offset uint64,
) (squashFSMetadataBlock, error) {
	if cache == nil || cache.decoder == nil {
		return squashFSMetadataBlock{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata cache is not initialized"),
		}
	}
	if err := checkSquashFSContext(cache.ctx); err != nil {
		return squashFSMetadataBlock{}, err
	}
	if block, ok := cache.blocks[offset]; ok {
		return block, nil
	}
	if _, ok := cache.starts[offset]; !ok {
		if err := cache.scanTo(offset); err != nil {
			return squashFSMetadataBlock{}, err
		}
		if block, ok := cache.blocks[offset]; ok {
			return block, nil
		}
	}
	block, err := cache.decoder.readBlock(cache.ctx, cache.region, offset, 0)
	if err != nil {
		return squashFSMetadataBlock{}, err
	}
	cache.remember(offset, block)
	return block, nil
}

func (cache *squashFSMetadataCache) scanTo(offset uint64) error {
	if offset < cache.region.start || offset >= cache.region.end {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata block offset %d is outside [%d, %d)",
				offset,
				cache.region.start,
				cache.region.end,
			),
		}
	}
	for cache.frontier <= offset {
		if cache.count >= squashFSMetadataBlockLimit {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS metadata block count exceeds %d",
					squashFSMetadataBlockLimit,
				),
			}
		}
		start := cache.frontier
		block, err := cache.decoder.readBlock(
			cache.ctx,
			cache.region,
			start,
			0,
		)
		if err != nil {
			return err
		}
		logicalEnd, ok := addSquashFSUint64(
			cache.bytes,
			uint64(len(block.data)),
		)
		if !ok {
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS decoded metadata size overflows"),
			}
		}
		cache.starts[start] = struct{}{}
		cache.logical[start] = cache.bytes
		cache.frontier = block.end
		cache.count++
		cache.bytes = logicalEnd
		cache.remember(start, block)
	}
	if _, ok := cache.starts[offset]; !ok {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata block offset %d is not a block boundary",
				offset,
			),
		}
	}
	return nil
}

func (cache *squashFSMetadataCache) validateComplete() error {
	if cache == nil || cache.decoder == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata cache is not initialized"),
		}
	}
	for cache.frontier < cache.region.end {
		if err := cache.scanTo(cache.frontier); err != nil {
			return err
		}
	}
	if cache.frontier != cache.region.end {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata blocks end at %d, want %d",
				cache.frontier,
				cache.region.end,
			),
		}
	}
	return nil
}

func (cache *squashFSMetadataCache) remember(
	offset uint64,
	block squashFSMetadataBlock,
) {
	if _, ok := cache.blocks[offset]; ok {
		return
	}
	if len(cache.order) == squashFSMetadataCacheEntries {
		delete(cache.blocks, cache.order[0])
		copy(cache.order, cache.order[1:])
		cache.order = cache.order[:len(cache.order)-1]
	}
	cache.blocks[offset] = block
	cache.order = append(cache.order, offset)
}

type squashFSCachedCursor struct {
	cache    *squashFSMetadataCache
	offset   uint64
	next     uint64
	block    []byte
	position int
}

func newSquashFSCachedCursor(
	cache *squashFSMetadataCache,
	offset uint64,
	position uint16,
) (*squashFSCachedCursor, error) {
	if cache == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata cache is nil"),
		}
	}
	block, err := cache.block(offset)
	if err != nil {
		return nil, err
	}
	if uint64(position) >= uint64(len(block.data)) {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata position = %d, block size = %d",
				position,
				len(block.data),
			),
		}
	}
	return &squashFSCachedCursor{
		cache:    cache,
		offset:   offset,
		next:     block.end,
		block:    block.data,
		position: int(position),
	}, nil
}

func (cursor *squashFSCachedCursor) readFull(destination []byte) error {
	if cursor == nil || cursor.cache == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata cursor is not initialized"),
		}
	}
	for len(destination) > 0 {
		if err := checkSquashFSContext(cursor.cache.ctx); err != nil {
			return err
		}
		if cursor.position == len(cursor.block) {
			offset := cursor.next
			block, err := cursor.cache.block(cursor.next)
			if err != nil {
				return err
			}
			cursor.offset = offset
			cursor.block = block.data
			cursor.position = 0
			cursor.next = block.end
		}
		count := copy(destination, cursor.block[cursor.position:])
		cursor.position += count
		destination = destination[count:]
	}
	return nil
}

func (cursor *squashFSCachedCursor) logicalPosition() (uint64, error) {
	if cursor == nil || cursor.cache == nil {
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata cursor is not initialized"),
		}
	}
	start, ok := cursor.cache.logical[cursor.offset]
	if !ok {
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf(
				"SquashFS metadata block %d has no logical offset",
				cursor.offset,
			),
		}
	}
	position, ok := addSquashFSUint64(start, uint64(cursor.position))
	if !ok {
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata logical position overflows"),
		}
	}
	return position, nil
}

type squashFSInodeDecoder struct {
	superblock       squashFSSuperblockFacts
	ids              []uint32
	inodes           *squashFSMetadataCache
	maxLogicalBytes  uint64
	logicalBytes     uint64
	retainedRawBytes uint64
	indexCount       uint64
	blockCount       uint64
	ranges           []squashFSMetadataRange
}

func newSquashFSInodeDecoder(
	ctx context.Context,
	decoder *squashFSMetadataDecoder,
	superblock squashFSSuperblockFacts,
	ids []uint32,
	maxLogicalBytes uint64,
) (*squashFSInodeDecoder, error) {
	if superblock.InodeTableStart < squashFSSuperblockSize ||
		superblock.InodeTableStart >= superblock.DirectoryTableStart ||
		superblock.DirectoryTableStart > superblock.FragmentTableStart ||
		superblock.FragmentTableStart > superblock.BytesUsed {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS table boundaries %d, %d, %d, %d are invalid",
				superblock.InodeTableStart,
				superblock.DirectoryTableStart,
				superblock.FragmentTableStart,
				superblock.BytesUsed,
			),
		}
	}
	if len(ids) == 0 {
		return nil, &artifactContentError{
			cause: fmt.Errorf("SquashFS ID table is empty"),
		}
	}
	cache, err := newSquashFSMetadataCache(
		ctx,
		decoder,
		squashFSRegion{
			start: superblock.InodeTableStart,
			end:   superblock.DirectoryTableStart,
		},
	)
	if err != nil {
		return nil, err
	}
	return &squashFSInodeDecoder{
		superblock:      superblock,
		ids:             ids,
		inodes:          cache,
		maxLogicalBytes: maxLogicalBytes,
	}, nil
}

func (decoder *squashFSInodeDecoder) read(
	reference uint64,
) (squashFSInodeFacts, error) {
	if decoder == nil || decoder.inodes == nil {
		return squashFSInodeFacts{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS inode decoder is not initialized"),
		}
	}
	if reference>>48 != 0 {
		return squashFSInodeFacts{}, &artifactContentError{
			cause: fmt.Errorf("SquashFS inode reference %#x is not canonical", reference),
		}
	}
	blockOffset := reference >> 16
	physicalBlock, ok := addSquashFSUint64(
		decoder.superblock.InodeTableStart,
		blockOffset,
	)
	if !ok || physicalBlock >= decoder.superblock.DirectoryTableStart {
		return squashFSInodeFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS inode block %d is outside the inode table",
				blockOffset,
			),
		}
	}
	cursor, err := newSquashFSCachedCursor(
		decoder.inodes,
		physicalBlock,
		uint16(reference),
	)
	if err != nil {
		return squashFSInodeFacts{}, fmt.Errorf(
			"open SquashFS inode reference %#x: %w",
			reference,
			err,
		)
	}
	rangeStart, err := cursor.logicalPosition()
	if err != nil {
		return squashFSInodeFacts{}, err
	}

	var base [16]byte
	if err := cursor.readFull(base[:]); err != nil {
		return squashFSInodeFacts{}, fmt.Errorf(
			"read SquashFS inode reference %#x: %w",
			reference,
			err,
		)
	}
	order := binary.LittleEndian
	facts := squashFSInodeFacts{
		Reference:   reference,
		Form:        order.Uint16(base[0:2]),
		Mode:        order.Uint16(base[2:4]),
		UIDIndex:    order.Uint16(base[4:6]),
		GIDIndex:    order.Uint16(base[6:8]),
		ModTimeUnix: order.Uint32(base[8:12]),
		InodeNumber: order.Uint32(base[12:16]),
		XattrIndex:  squashFSInvalidXattr,
	}
	if int(facts.UIDIndex) >= len(decoder.ids) ||
		int(facts.GIDIndex) >= len(decoder.ids) {
		return squashFSInodeFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS inode %d ID indexes %d, %d exceed %d IDs",
				facts.InodeNumber,
				facts.UIDIndex,
				facts.GIDIndex,
				len(decoder.ids),
			),
		}
	}
	facts.UID = decoder.ids[facts.UIDIndex]
	facts.GID = decoder.ids[facts.GIDIndex]

	switch facts.Form {
	case squashFSBasicDirectoryForm:
		facts.Kind = squashFSDirectoryKind
		var encoded [16]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[4:8])
		facts.Directory = &squashFSDirectoryFacts{
			StartBlock:  order.Uint32(encoded[0:4]),
			EncodedSize: uint32(order.Uint16(encoded[8:10])),
			Offset:      order.Uint16(encoded[10:12]),
			ParentInode: order.Uint32(encoded[12:16]),
		}
	case squashFSExtendedDirectoryForm:
		facts.Kind = squashFSDirectoryKind
		var encoded [24]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[0:4])
		facts.XattrIndex = order.Uint32(encoded[20:24])
		facts.Directory = &squashFSDirectoryFacts{
			EncodedSize: order.Uint32(encoded[4:8]),
			StartBlock:  order.Uint32(encoded[8:12]),
			ParentInode: order.Uint32(encoded[12:16]),
			Offset:      order.Uint16(encoded[18:20]),
		}
		indexCount := uint64(order.Uint16(encoded[16:18]))
		if decoder.indexCount > maxArtifactEntries-indexCount {
			return squashFSInodeFacts{}, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS directory index count exceeds %d",
					maxArtifactEntries,
				),
			}
		}
		decoder.indexCount += indexCount
		facts.Directory.Indexes = make(
			[]squashFSDirectoryIndexFacts,
			0,
			int(indexCount),
		)
		for range indexCount {
			if err := checkSquashFSContext(decoder.inodes.ctx); err != nil {
				return squashFSInodeFacts{}, err
			}
			var indexHeader [12]byte
			if err := cursor.readFull(indexHeader[:]); err != nil {
				return squashFSInodeFacts{}, err
			}
			nameSize := uint64(order.Uint32(indexHeader[8:12])) + 1
			if nameSize > 256 {
				return squashFSInodeFacts{}, &artifactContentError{
					cause: fmt.Errorf(
						"SquashFS directory index name size = %d, want at most 256",
						nameSize,
					),
				}
			}
			var name [256]byte
			if err := cursor.readFull(name[:nameSize]); err != nil {
				return squashFSInodeFacts{}, err
			}
			facts.Directory.Indexes = append(
				facts.Directory.Indexes,
				squashFSDirectoryIndexFacts{
					Index:      order.Uint32(indexHeader[0:4]),
					StartBlock: order.Uint32(indexHeader[4:8]),
					Name:       string(name[:nameSize]),
				},
			)
		}
	case squashFSBasicRegularForm:
		facts.Kind = squashFSRegularKind
		facts.LinkCount = 1
		var encoded [16]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.Regular = &squashFSRegularFacts{
			StartBlock: uint64(order.Uint32(encoded[0:4])),
			Fragment:   order.Uint32(encoded[4:8]),
			Offset:     order.Uint32(encoded[8:12]),
			Size:       uint64(order.Uint32(encoded[12:16])),
		}
		if err := decoder.readRegularBlocks(cursor, facts.Regular); err != nil {
			return squashFSInodeFacts{}, err
		}
	case squashFSExtendedRegularForm:
		facts.Kind = squashFSRegularKind
		var encoded [40]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[24:28])
		facts.XattrIndex = order.Uint32(encoded[36:40])
		facts.Regular = &squashFSRegularFacts{
			StartBlock:  order.Uint64(encoded[0:8]),
			Size:        order.Uint64(encoded[8:16]),
			SparseBytes: order.Uint64(encoded[16:24]),
			Fragment:    order.Uint32(encoded[28:32]),
			Offset:      order.Uint32(encoded[32:36]),
		}
		if facts.Regular.StartBlock > math.MaxInt64 ||
			facts.Regular.Size > math.MaxInt64 ||
			facts.Regular.SparseBytes > math.MaxInt64 {
			return squashFSInodeFacts{}, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS extended regular inode %d has out-of-range signed fields",
					facts.InodeNumber,
				),
			}
		}
		if err := decoder.readRegularBlocks(cursor, facts.Regular); err != nil {
			return squashFSInodeFacts{}, err
		}
	case squashFSBasicSymlinkForm, squashFSExtendedSymlinkForm:
		facts.Kind = squashFSSymlinkKind
		var encoded [8]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[0:4])
		targetSize := uint64(order.Uint32(encoded[4:8]))
		if targetSize > maxSymlinkTargetBytes ||
			decoder.retainedRawBytes > uint64(maxArtifactNameBytes)-targetSize {
			return squashFSInodeFacts{}, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS symbolic-link target exceeds retained-byte bounds",
				),
			}
		}
		decoder.retainedRawBytes += targetSize
		var target [maxSymlinkTargetBytes]byte
		if err := cursor.readFull(target[:targetSize]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkTarget = string(target[:targetSize])
		if facts.Form == squashFSExtendedSymlinkForm {
			var xattr [4]byte
			if err := cursor.readFull(xattr[:]); err != nil {
				return squashFSInodeFacts{}, err
			}
			facts.XattrIndex = order.Uint32(xattr[:])
		}
	case squashFSBasicBlockDeviceForm, squashFSBasicCharDeviceForm:
		if facts.Form == squashFSBasicBlockDeviceForm {
			facts.Kind = squashFSBlockDeviceKind
		} else {
			facts.Kind = squashFSCharDeviceKind
		}
		var encoded [8]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[0:4])
		facts.Device = order.Uint32(encoded[4:8])
	case squashFSExtendedBlockForm, squashFSExtendedCharForm:
		if facts.Form == squashFSExtendedBlockForm {
			facts.Kind = squashFSBlockDeviceKind
		} else {
			facts.Kind = squashFSCharDeviceKind
		}
		var encoded [12]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[0:4])
		facts.Device = order.Uint32(encoded[4:8])
		facts.XattrIndex = order.Uint32(encoded[8:12])
	case squashFSBasicFIFOForm, squashFSBasicSocketForm:
		if facts.Form == squashFSBasicFIFOForm {
			facts.Kind = squashFSFIFODeviceKind
		} else {
			facts.Kind = squashFSSocketKind
		}
		var encoded [4]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[:])
	case squashFSExtendedFIFOForm, squashFSExtendedSocketForm:
		if facts.Form == squashFSExtendedFIFOForm {
			facts.Kind = squashFSFIFODeviceKind
		} else {
			facts.Kind = squashFSSocketKind
		}
		var encoded [8]byte
		if err := cursor.readFull(encoded[:]); err != nil {
			return squashFSInodeFacts{}, err
		}
		facts.LinkCount = order.Uint32(encoded[0:4])
		facts.XattrIndex = order.Uint32(encoded[4:8])
	default:
		return squashFSInodeFacts{}, &artifactContentError{
			cause: fmt.Errorf("SquashFS inode form = %d is unknown", facts.Form),
		}
	}
	rangeEnd, err := cursor.logicalPosition()
	if err != nil {
		return squashFSInodeFacts{}, err
	}
	decoder.ranges = append(
		decoder.ranges,
		squashFSMetadataRange{start: rangeStart, end: rangeEnd},
	)
	return facts, nil
}

func (decoder *squashFSInodeDecoder) readRegularBlocks(
	cursor *squashFSCachedCursor,
	regular *squashFSRegularFacts,
) error {
	if decoder == nil || regular == nil || cursor == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS regular inode decoder is not initialized"),
		}
	}
	blockSize := uint64(decoder.superblock.BlockSize)
	if blockSize == 0 {
		return &artifactContentError{
			cause: fmt.Errorf("SquashFS block size is zero"),
		}
	}
	if regular.Size > uint64(maxArtifactFileSize) ||
		regular.Size > decoder.maxLogicalBytes ||
		decoder.logicalBytes > decoder.maxLogicalBytes-regular.Size {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS regular-file logical bytes exceed configured bounds",
			),
		}
	}
	decoder.logicalBytes += regular.Size

	blockCount := regular.Size / blockSize
	if regular.Fragment == squashFSInvalidFragment &&
		regular.Size%blockSize != 0 {
		blockCount++
	}
	descriptorLimit := uint64(maxArtifactEntries)
	additional, ok := ceilSquashFSDivide(
		decoder.maxLogicalBytes,
		squashFSDataBlockSize,
	)
	if !ok || descriptorLimit > math.MaxUint64-additional {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("derive SquashFS block descriptor bound: overflow"),
		}
	}
	descriptorLimit += additional
	if blockCount > descriptorLimit ||
		decoder.blockCount > descriptorLimit-blockCount {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS block descriptor count exceeds %d",
				descriptorLimit,
			),
		}
	}
	decoder.blockCount += blockCount
	regular.Blocks = make([]squashFSBlockFacts, 0, int(blockCount))
	physical := regular.StartBlock
	logicalRemaining := regular.Size
	for position := uint64(0); position < blockCount; position++ {
		if err := checkSquashFSContext(decoder.inodes.ctx); err != nil {
			return err
		}
		var encodedBytes [4]byte
		if err := cursor.readFull(encodedBytes[:]); err != nil {
			return err
		}
		encoded := binary.LittleEndian.Uint32(encodedBytes[:])
		logicalSize := min(logicalRemaining, blockSize)
		block := squashFSBlockFacts{
			Encoded:     encoded,
			LogicalSize: uint32(logicalSize),
		}
		if encoded == 0 {
			block.Sparse = true
			regular.Blocks = append(regular.Blocks, block)
			logicalRemaining -= logicalSize
			continue
		}

		block.Uncompressed = encoded&squashFSDataUncompressedBit != 0
		block.StoredSize = encoded &^ squashFSDataUncompressedBit
		if block.StoredSize == 0 || block.StoredSize > squashFSDataBlockSize {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS regular-file block %d stored size = %d, want within [1, %d]",
					position,
					block.StoredSize,
					squashFSDataBlockSize,
				),
			}
		}
		block.Start = physical
		end, ok := addSquashFSUint64(physical, uint64(block.StoredSize))
		if !ok || physical < squashFSSuperblockSize ||
			end > decoder.superblock.InodeTableStart {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS regular-file block %d extent [%d, %d) is outside data storage",
					position,
					physical,
					end,
				),
			}
		}
		block.End = end
		physical = end
		regular.Blocks = append(regular.Blocks, block)
		logicalRemaining -= logicalSize
	}
	return nil
}

func ceilSquashFSDivide(value, divisor uint64) (uint64, bool) {
	if divisor == 0 {
		return 0, false
	}
	quotient := value / divisor
	if value%divisor == 0 {
		return quotient, true
	}
	if quotient == math.MaxUint64 {
		return 0, false
	}
	return quotient + 1, true
}
