package deployment

import (
	"context"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

type squashFSDataReader struct {
	source             io.ReaderAt
	region             squashFSRegion
	files              map[string]squashFSRegularFacts
	hasOverlappingData bool
}

func newSquashFSDataReader(
	ctx context.Context,
	source io.ReaderAt,
	superblock squashFSSuperblockFacts,
	tree squashFSTreeFacts,
) (*squashFSDataReader, error) {
	if source == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS data source is nil"),
		}
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	if superblock.InodeTableStart < squashFSSuperblockSize {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS data region [%d, %d) is invalid",
				squashFSSuperblockSize,
				superblock.InodeTableStart,
			),
		}
	}

	reader := &squashFSDataReader{
		source: source,
		region: squashFSRegion{
			start: squashFSSuperblockSize,
			end:   superblock.InodeTableStart,
		},
		files:              make(map[string]squashFSRegularFacts),
		hasOverlappingData: tree.HasOverlappingData,
	}
	var decoder *zstd.Decoder
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	inodes := make(map[uint64]squashFSRegularFacts)
	for _, path := range tree.Paths {
		if err := checkSquashFSContext(ctx); err != nil {
			return nil, err
		}
		if path.Inode == nil {
			return nil, &artifactInfrastructureError{
				cause: fmt.Errorf("SquashFS path %q has no inode", path.Path),
			}
		}
		if path.Inode.Kind != squashFSRegularKind {
			continue
		}
		if path.Inode.Regular == nil {
			return nil, &artifactInfrastructureError{
				cause: fmt.Errorf("SquashFS path %q has no regular-file facts", path.Path),
			}
		}
		regular, retained := inodes[path.Inode.Reference]
		if !retained {
			regular = cloneSquashFSRegularFacts(*path.Inode.Regular)
			if err := validateSquashFSRegularDescriptors(path.Inode.Form, regular); err != nil {
				return nil, fmt.Errorf("validate SquashFS path %q descriptors: %w", path.Path, err)
			}
			for position, block := range regular.Blocks {
				if block.Sparse || reader.hasOverlappingData {
					continue
				}
				if !block.Uncompressed && decoder == nil {
					var err error
					decoder, err = newSquashFSDataDecoder()
					if err != nil {
						return nil, err
					}
				}
				if err := validateSquashFSDataBlock(
					ctx,
					source,
					reader.region,
					decoder,
					block,
				); err != nil {
					return nil, fmt.Errorf(
						"validate SquashFS path %q block %d: %w",
						path.Path,
						position,
						err,
					)
				}
			}
			inodes[path.Inode.Reference] = regular
		}
		if _, exists := reader.files[path.Path]; exists {
			return nil, &artifactInfrastructureError{
				cause: fmt.Errorf("SquashFS path %q is retained twice", path.Path),
			}
		}
		reader.files[path.Path] = regular
	}
	return reader, nil
}

func (reader *squashFSDataReader) Open(
	ctx context.Context,
	path string,
) (io.ReadCloser, error) {
	if reader == nil || reader.source == nil || reader.files == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS data reader is not initialized"),
		}
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	if reader.hasOverlappingData {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS overlapping data reached file open"),
		}
	}
	regular, ok := reader.files[path]
	if !ok {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS regular path %q is not retained", path),
		}
	}
	if regular.Fragment != squashFSInvalidFragment {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS regular path %q requires a fragment", path),
		}
	}
	return &squashFSFileReader{
		ctx:     ctx,
		source:  reader.source,
		region:  reader.region,
		regular: regular,
	}, nil
}

type squashFSFileReader struct {
	ctx         context.Context
	source      io.ReaderAt
	region      squashFSRegion
	regular     squashFSRegularFacts
	decoder     *zstd.Decoder
	block       []byte
	blockIndex  int
	blockOffset uint32
	logicalRead uint64
	closed      bool
}

func (reader *squashFSFileReader) Read(destination []byte) (int, error) {
	if reader == nil || reader.source == nil {
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS file reader is not initialized"),
		}
	}
	if reader.closed {
		return 0, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS file reader is closed"),
		}
	}
	if err := checkSquashFSContext(reader.ctx); err != nil {
		return 0, err
	}
	if len(destination) == 0 {
		if reader.logicalRead == reader.regular.Size {
			return 0, io.EOF
		}
		return 0, nil
	}
	if reader.logicalRead == reader.regular.Size {
		return 0, io.EOF
	}

	written := 0
	for written < len(destination) && reader.logicalRead < reader.regular.Size {
		if reader.blockIndex >= len(reader.regular.Blocks) {
			return written, &artifactInfrastructureError{
				cause: fmt.Errorf(
					"SquashFS path ended after %d of %d logical bytes",
					reader.logicalRead,
					reader.regular.Size,
				),
			}
		}
		descriptor := reader.regular.Blocks[reader.blockIndex]
		if reader.blockOffset == 0 && !descriptor.Sparse {
			block, err := reader.readBlock(descriptor)
			if err != nil {
				return written, err
			}
			reader.block = block
		}
		remaining := int(descriptor.LogicalSize - reader.blockOffset)
		count := min(len(destination)-written, remaining)
		if descriptor.Sparse {
			clear(destination[written : written+count])
		} else {
			copy(
				destination[written:written+count],
				reader.block[int(reader.blockOffset):int(reader.blockOffset)+count],
			)
		}
		written += count
		reader.blockOffset += uint32(count)
		reader.logicalRead += uint64(count)
		if reader.blockOffset == descriptor.LogicalSize {
			reader.block = nil
			reader.blockIndex++
			reader.blockOffset = 0
		}
	}
	return written, nil
}

func (reader *squashFSFileReader) Close() error {
	if reader == nil {
		return nil
	}
	if reader.decoder != nil {
		reader.decoder.Close()
		reader.decoder = nil
	}
	reader.block = nil
	reader.closed = true
	return nil
}

func (reader *squashFSFileReader) readBlock(
	block squashFSBlockFacts,
) ([]byte, error) {
	if block.Uncompressed {
		stored := make([]byte, int(block.StoredSize))
		if err := readSquashFSRegionAt(
			reader.ctx,
			reader.source,
			reader.region,
			stored,
			block.Start,
		); err != nil {
			return nil, fmt.Errorf("read SquashFS data block: %w", err)
		}
		return stored, nil
	}
	if reader.decoder == nil {
		decoder, err := newSquashFSDataDecoder()
		if err != nil {
			return nil, err
		}
		reader.decoder = decoder
	}
	return decodeSquashFSDataBlock(
		reader.ctx,
		reader.source,
		reader.region,
		reader.decoder,
		block,
	)
}

func validateSquashFSRegularDescriptors(
	form uint16,
	regular squashFSRegularFacts,
) error {
	var logical uint64
	var sparse uint64
	for position, block := range regular.Blocks {
		if block.LogicalSize == 0 || block.LogicalSize > squashFSDataBlockSize {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS block %d logical size = %d, want within [1, %d]",
					position,
					block.LogicalSize,
					squashFSDataBlockSize,
				),
			}
		}
		next, ok := addSquashFSUint64(logical, uint64(block.LogicalSize))
		if !ok || next > regular.Size {
			return &artifactContentError{
				cause: fmt.Errorf("SquashFS block %d exceeds the regular-file size", position),
			}
		}
		logical = next
		if block.Sparse {
			sparse += uint64(block.LogicalSize)
			if block.Encoded != 0 || block.StoredSize != 0 ||
				block.Start != 0 || block.End != 0 || block.Uncompressed {
				return &artifactInfrastructureError{
					cause: fmt.Errorf("SquashFS sparse block %d descriptor is inconsistent", position),
				}
			}
			continue
		}
		if block.StoredSize == 0 || block.StoredSize > squashFSDataBlockSize {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS block %d stored size = %d, want within [1, %d]",
					position,
					block.StoredSize,
					squashFSDataBlockSize,
				),
			}
		}
		if block.End < block.Start ||
			block.End-block.Start != uint64(block.StoredSize) {
			return &artifactInfrastructureError{
				cause: fmt.Errorf("SquashFS data block %d descriptor is inconsistent", position),
			}
		}
		if block.Uncompressed && block.StoredSize != block.LogicalSize {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS uncompressed block %d stored size = %d, want %d",
					position,
					block.StoredSize,
					block.LogicalSize,
				),
			}
		}
	}
	if regular.Fragment == squashFSInvalidFragment && logical != regular.Size {
		return &artifactInfrastructureError{
			cause: fmt.Errorf(
				"SquashFS regular-file descriptors cover %d of %d logical bytes",
				logical,
				regular.Size,
			),
		}
	}
	switch form {
	case squashFSExtendedRegularForm:
		expectedSparse := sparse
		if regular.Size > 0 && sparse == regular.Size {
			expectedSparse--
		}
		if regular.SparseBytes != expectedSparse {
			return &artifactContentError{
				cause: fmt.Errorf(
					"SquashFS regular-file sparse bytes = %d, want %d",
					regular.SparseBytes,
					expectedSparse,
				),
			}
		}
	case squashFSBasicRegularForm:
	default:
		return &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS regular-file form %d is inconsistent", form),
		}
	}
	return nil
}

func validateSquashFSDataBlock(
	ctx context.Context,
	source io.ReaderAt,
	region squashFSRegion,
	decoder *zstd.Decoder,
	block squashFSBlockFacts,
) error {
	if block.Uncompressed {
		stored := make([]byte, int(block.StoredSize))
		if err := readSquashFSRegionAt(ctx, source, region, stored, block.Start); err != nil {
			return fmt.Errorf("read SquashFS data block: %w", err)
		}
		return nil
	}
	_, err := decodeSquashFSDataBlock(ctx, source, region, decoder, block)
	return err
}

func decodeSquashFSDataBlock(
	ctx context.Context,
	source io.ReaderAt,
	region squashFSRegion,
	decoder *zstd.Decoder,
	block squashFSBlockFacts,
) ([]byte, error) {
	if decoder == nil {
		return nil, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS data decoder is not initialized"),
		}
	}
	stored := make([]byte, int(block.StoredSize))
	if err := readSquashFSRegionAt(ctx, source, region, stored, block.Start); err != nil {
		return nil, fmt.Errorf("read SquashFS data block: %w", err)
	}
	if err := validateSquashFSZstdFrame(stored, uint64(block.LogicalSize)); err != nil {
		return nil, err
	}
	if err := checkSquashFSContext(ctx); err != nil {
		return nil, err
	}
	decoded, err := decoder.DecodeAll(
		stored,
		make([]byte, 0, int(block.LogicalSize)),
	)
	if err != nil {
		return nil, &artifactContentError{
			cause: fmt.Errorf("decode SquashFS data: %w", err),
		}
	}
	if len(decoded) != int(block.LogicalSize) {
		return nil, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS decoded data size = %d, want %d",
				len(decoded),
				block.LogicalSize,
			),
		}
	}
	return decoded, nil
}

func newSquashFSDataDecoder() (*zstd.Decoder, error) {
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
			cause: fmt.Errorf("create SquashFS data decoder: %w", err),
		}
	}
	return decoder, nil
}

func cloneSquashFSRegularFacts(regular squashFSRegularFacts) squashFSRegularFacts {
	regular.Blocks = append([]squashFSBlockFacts(nil), regular.Blocks...)
	return regular
}
