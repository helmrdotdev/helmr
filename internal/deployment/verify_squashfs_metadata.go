package deployment

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
)

const (
	squashFSMetadataBlockSize = 8192
	squashFSZstdWindowSize    = 131072
)

type squashFSRegion struct {
	start uint64
	end   uint64
}

type squashFSMetadataBlock struct {
	data []byte
	end  uint64
}

type squashFSMetadataDecoder struct {
	reader  io.ReaderAt
	decoder *zstd.Decoder
}

func newSquashFSMetadataDecoder(reader io.ReaderAt) (*squashFSMetadataDecoder, error) {
	if reader == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata reader is nil"),
		}
	}
	decoder, err := zstd.NewReader(
		nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(squashFSZstdWindowSize),
		zstd.WithDecoderMaxWindow(squashFSZstdWindowSize),
		zstd.WithDecodeAllCapLimit(true),
	)
	if err != nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("create SquashFS Zstandard decoder: %w", err),
		}
	}
	return &squashFSMetadataDecoder{
		reader:  reader,
		decoder: decoder,
	}, nil
}

func (decoder *squashFSMetadataDecoder) Close() {
	if decoder != nil && decoder.decoder != nil {
		decoder.decoder.Close()
		decoder.decoder = nil
	}
}

func (decoder *squashFSMetadataDecoder) readBlock(
	ctx context.Context,
	region squashFSRegion,
	offset uint64,
	expectedSize uint64,
) (squashFSMetadataBlock, error) {
	if decoder == nil || decoder.reader == nil || decoder.decoder == nil {
		return squashFSMetadataBlock{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata decoder is not initialized"),
		}
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return squashFSMetadataBlock{}, err
	}
	if region.start > region.end {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata region [%d, %d) is invalid",
				region.start,
				region.end,
			),
		}
	}
	if expectedSize > squashFSMetadataBlockSize {
		return squashFSMetadataBlock{}, &artifactInfrastructureError{
			cause: fmt.Errorf(
				"SquashFS expected metadata size = %d, want at most %d",
				expectedSize,
				squashFSMetadataBlockSize,
			),
		}
	}

	var encodedHeader [2]byte
	if err := readSquashFSRegionAt(
		ctx,
		decoder.reader,
		region,
		encodedHeader[:],
		offset,
	); err != nil {
		return squashFSMetadataBlock{}, fmt.Errorf("read SquashFS metadata header: %w", err)
	}

	header := binary.LittleEndian.Uint16(encodedHeader[:])
	storedSize := uint64(header & 0x7fff)
	if storedSize == 0 || storedSize > squashFSMetadataBlockSize {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS metadata stored size = %d, want within [1, %d]",
				storedSize,
				squashFSMetadataBlockSize,
			),
		}
	}
	payloadOffset, ok := addSquashFSUint64(offset, uint64(len(encodedHeader)))
	if !ok {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf("advance SquashFS metadata offset %d: overflow", offset),
		}
	}
	blockEnd, ok := addSquashFSUint64(payloadOffset, storedSize)
	if !ok {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf(
				"advance SquashFS metadata payload offset %d by %d: overflow",
				payloadOffset,
				storedSize,
			),
		}
	}

	stored := make([]byte, int(storedSize))
	if err := readSquashFSRegionAt(
		ctx,
		decoder.reader,
		region,
		stored,
		payloadOffset,
	); err != nil {
		return squashFSMetadataBlock{}, fmt.Errorf("read SquashFS metadata payload: %w", err)
	}

	var decoded []byte
	if header&0x8000 != 0 {
		decoded = stored
	} else {
		limit := uint64(squashFSMetadataBlockSize)
		if expectedSize > 0 {
			limit = expectedSize
		}
		if err := validateSquashFSZstdFrame(stored, limit); err != nil {
			return squashFSMetadataBlock{}, err
		}
		if err := checkSquashFSContext(ctx); err != nil {
			return squashFSMetadataBlock{}, err
		}
		var err error
		decoded, err = decoder.decoder.DecodeAll(stored, make([]byte, 0, int(limit)))
		if err != nil {
			return squashFSMetadataBlock{}, &artifactContentError{
				cause: fmt.Errorf("decode SquashFS metadata: %w", err),
			}
		}
	}

	if len(decoded) == 0 || len(decoded) > squashFSMetadataBlockSize {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS decoded metadata size = %d, want within [1, %d]",
				len(decoded),
				squashFSMetadataBlockSize,
			),
		}
	}
	if expectedSize > 0 && uint64(len(decoded)) != expectedSize {
		return squashFSMetadataBlock{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS decoded metadata size = %d, want %d",
				len(decoded),
				expectedSize,
			),
		}
	}

	return squashFSMetadataBlock{data: decoded, end: blockEnd}, nil
}

func readSquashFSIDTable(
	ctx context.Context,
	decoder *squashFSMetadataDecoder,
	dataStart uint64,
	indexStart uint64,
	bytesUsed uint64,
	idCount uint16,
) ([]uint32, error) {
	if decoder == nil || decoder.reader == nil || decoder.decoder == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS metadata decoder is not initialized"),
		}
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	if dataStart > indexStart || indexStart > bytesUsed {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS ID table boundaries %d, %d, %d are invalid",
				dataStart,
				indexStart,
				bytesUsed,
			),
		}
	}

	logicalBytes := uint64(idCount) * 4
	blockCount := (logicalBytes + squashFSMetadataBlockSize - 1) /
		squashFSMetadataBlockSize
	indexBytes := blockCount * 8
	indexEnd, ok := addSquashFSUint64(indexStart, indexBytes)
	if !ok || indexEnd != bytesUsed {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS ID index [%d, %d) does not end at bytes_used %d",
				indexStart,
				indexEnd,
				bytesUsed,
			),
		}
	}
	if blockCount == 0 {
		if dataStart != indexStart {
			return nil, &artifactContentError{
				cause: fmt.Errorf(
					"empty SquashFS ID table data range = [%d, %d)",
					dataStart,
					indexStart,
				),
			}
		}
		return []uint32{}, nil
	}

	encodedPointers := make([]byte, int(indexBytes))
	if err := readSquashFSRegionAt(
		ctx,
		decoder.reader,
		squashFSRegion{start: indexStart, end: bytesUsed},
		encodedPointers,
		indexStart,
	); err != nil {
		return nil, fmt.Errorf("read SquashFS ID index: %w", err)
	}
	pointers := make([]uint64, int(blockCount))
	for index := range pointers {
		pointers[index] = binary.LittleEndian.Uint64(
			encodedPointers[index*8 : index*8+8],
		)
		if pointers[index] < dataStart || pointers[index] >= indexStart {
			return nil, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS ID block %d offset %d is outside [%d, %d)",
					index,
					pointers[index],
					dataStart,
					indexStart,
				),
			}
		}
	}
	if pointers[0] != dataStart {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"first SquashFS ID block offset = %d, want %d",
				pointers[0],
				dataStart,
			),
		}
	}

	values := make([]uint32, 0, int(idCount))
	remaining := logicalBytes
	for index, pointer := range pointers {
		expectedSize := min(remaining, uint64(squashFSMetadataBlockSize))
		block, err := decoder.readBlock(
			ctx,
			squashFSRegion{start: dataStart, end: indexStart},
			pointer,
			expectedSize,
		)
		if err != nil {
			return nil, fmt.Errorf("read SquashFS ID block %d: %w", index, err)
		}
		expectedEnd := indexStart
		if index+1 < len(pointers) {
			expectedEnd = pointers[index+1]
		}
		if block.end != expectedEnd {
			return nil, &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS ID block %d ends at %d, want %d",
					index,
					block.end,
					expectedEnd,
				),
			}
		}
		for position := 0; position < len(block.data); position += 4 {
			values = append(
				values,
				binary.LittleEndian.Uint32(block.data[position:position+4]),
			)
		}
		remaining -= expectedSize
	}
	return values, nil
}

func validateSquashFSZstdFrame(encoded []byte, outputLimit uint64) error {
	var header zstd.Header
	if err := header.Decode(encoded); err != nil {
		return &artifactContentError{
			cause: fmt.Errorf("decode SquashFS Zstandard header: %w", err),
		}
	}
	if header.Skippable {
		return &artifactContentError{
			cause: fmt.Errorf("SquashFS Zstandard frame is skippable"),
		}
	}
	if header.DictionaryID != 0 {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS Zstandard dictionary ID = %d, want 0",
				header.DictionaryID,
			),
		}
	}
	windowSize := header.WindowSize
	if header.SingleSegment {
		windowSize = header.FrameContentSize
	}
	if windowSize > squashFSZstdWindowSize {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS Zstandard window = %d, want at most %d",
				windowSize,
				squashFSZstdWindowSize,
			),
		}
	}
	if header.HasFCS && header.FrameContentSize > outputLimit {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS Zstandard content size = %d, want at most %d",
				header.FrameContentSize,
				outputLimit,
			),
		}
	}

	position := uint64(header.HeaderSize)
	for {
		blockHeaderEnd, ok := addSquashFSUint64(position, 3)
		if !ok || blockHeaderEnd > uint64(len(encoded)) {
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS Zstandard block header is truncated"),
			}
		}
		blockHeader := uint32(encoded[position]) |
			uint32(encoded[position+1])<<8 |
			uint32(encoded[position+2])<<16
		position = blockHeaderEnd
		last := blockHeader&1 != 0
		blockType := (blockHeader >> 1) & 3
		storedSize := uint64(blockHeader >> 3)
		switch blockType {
		case 0, 2:
		case 1:
			storedSize = 1
		default:
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS Zstandard block uses reserved type"),
			}
		}
		blockEnd, ok := addSquashFSUint64(position, storedSize)
		if !ok || blockEnd > uint64(len(encoded)) {
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS Zstandard block payload is truncated"),
			}
		}
		position = blockEnd
		if last {
			break
		}
	}
	if header.HasCheckSum {
		checksumEnd, ok := addSquashFSUint64(position, 4)
		if !ok || checksumEnd > uint64(len(encoded)) {
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS Zstandard checksum is truncated"),
			}
		}
		position = checksumEnd
	}
	if position != uint64(len(encoded)) {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS Zstandard frame consumes %d of %d bytes",
				position,
				len(encoded),
			),
		}
	}
	return nil
}

func readSquashFSRegionAt(
	ctx context.Context,
	reader io.ReaderAt,
	region squashFSRegion,
	destination []byte,
	offset uint64,
) error {
	if err := checkSquashFSContext(ctx); err != nil {
		return err
	}
	if reader == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS reader is nil"),
		}
	}
	if region.start > region.end || offset < region.start {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS read offset %d is outside [%d, %d)",
				offset,
				region.start,
				region.end,
			),
		}
	}
	end, ok := addSquashFSUint64(offset, uint64(len(destination)))
	if !ok || end > region.end || offset > math.MaxInt64 {
		return &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS read [%d, %d) is outside [%d, %d)",
				offset,
				end,
				region.start,
				region.end,
			),
		}
	}
	return readSquashFSAt(reader, destination, int64(offset))
}

func checkSquashFSContext(ctx context.Context) error {
	if ctx == nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS context is nil"),
		}
	}
	if err := ctx.Err(); err != nil {
		return &artifactInfrastructureError{
			cause: fmt.Errorf("read SquashFS: %w", err),
		}
	}
	return nil
}

func addSquashFSUint64(left, right uint64) (uint64, bool) {
	if left > math.MaxUint64-right {
		return 0, false
	}
	return left + right, true
}
