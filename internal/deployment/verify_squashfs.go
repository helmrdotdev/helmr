package deployment

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	squashFSMagic               = 0x73717368
	squashFSZstandardCompressor = 6
	squashFSV0Flags             = 0x0210
	squashFSSuperblockSize      = 96
	squashFSPhysicalAlign       = 4096
)

type squashFSSuperblockFacts struct {
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
	IDTableStart        uint64
	XattrIDTableStart   uint64
	InodeTableStart     uint64
	DirectoryTableStart uint64
	FragmentTableStart  uint64
	ExportTableStart    uint64
}

type squashFSTailFacts struct {
	ExpectedPhysicalSize uint64
	ExpectedPaddingBytes uint16
	HasExactPhysicalSize bool
	HasZeroPadding       bool
}

type squashFSFacts struct {
	Superblock   squashFSSuperblockFacts
	PhysicalSize uint64
	Tail         squashFSTailFacts
}

type artifactContentError struct {
	cause error
}

func (err *artifactContentError) Error() string {
	return err.cause.Error()
}

func (err *artifactContentError) Unwrap() error {
	return err.cause
}

type artifactInfrastructureError struct {
	cause error
}

func (err *artifactInfrastructureError) Error() string {
	return err.cause.Error()
}

func (err *artifactInfrastructureError) Unwrap() error {
	return err.cause
}

func readSquashFSFacts(reader io.ReaderAt, physicalSize int64) (squashFSFacts, error) {
	if reader == nil {
		return squashFSFacts{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS reader is nil"),
		}
	}
	if physicalSize < 0 {
		return squashFSFacts{}, &artifactInfrastructureError{
			cause: fmt.Errorf("SquashFS physical size = %d", physicalSize),
		}
	}
	if physicalSize < squashFSSuperblockSize {
		return squashFSFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS physical size = %d, want at least %d",
				physicalSize,
				squashFSSuperblockSize,
			),
		}
	}

	var encoded [squashFSSuperblockSize]byte
	if err := readSquashFSAt(reader, encoded[:], 0); err != nil {
		return squashFSFacts{}, fmt.Errorf("read SquashFS superblock: %w", err)
	}

	littleEndian := binary.LittleEndian
	superblock := squashFSSuperblockFacts{
		Magic:               littleEndian.Uint32(encoded[0:4]),
		InodeCount:          littleEndian.Uint32(encoded[4:8]),
		CreatedAtUnix:       littleEndian.Uint32(encoded[8:12]),
		BlockSize:           littleEndian.Uint32(encoded[12:16]),
		FragmentCount:       littleEndian.Uint32(encoded[16:20]),
		Compressor:          littleEndian.Uint16(encoded[20:22]),
		BlockLog:            littleEndian.Uint16(encoded[22:24]),
		Flags:               littleEndian.Uint16(encoded[24:26]),
		IDCount:             littleEndian.Uint16(encoded[26:28]),
		Major:               littleEndian.Uint16(encoded[28:30]),
		Minor:               littleEndian.Uint16(encoded[30:32]),
		RootInodeReference:  littleEndian.Uint64(encoded[32:40]),
		BytesUsed:           littleEndian.Uint64(encoded[40:48]),
		IDTableStart:        littleEndian.Uint64(encoded[48:56]),
		XattrIDTableStart:   littleEndian.Uint64(encoded[56:64]),
		InodeTableStart:     littleEndian.Uint64(encoded[64:72]),
		DirectoryTableStart: littleEndian.Uint64(encoded[72:80]),
		FragmentTableStart:  littleEndian.Uint64(encoded[80:88]),
		ExportTableStart:    littleEndian.Uint64(encoded[88:96]),
	}
	if superblock.BytesUsed < squashFSSuperblockSize ||
		superblock.BytesUsed > uint64(physicalSize) {
		return squashFSFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"SquashFS bytes_used = %d, want within [%d, %d]",
				superblock.BytesUsed,
				squashFSSuperblockSize,
				physicalSize,
			),
		}
	}

	expectedPhysicalSize, ok := roundUpSquashFSSize(
		superblock.BytesUsed,
		squashFSPhysicalAlign,
	)
	if !ok {
		return squashFSFacts{}, &artifactContentError{
			cause: fmt.Errorf(
				"round SquashFS bytes_used = %d to %d-byte boundary: overflow",
				superblock.BytesUsed,
				squashFSPhysicalAlign,
			),
		}
	}
	expectedPaddingBytes := expectedPhysicalSize - superblock.BytesUsed
	tail := squashFSTailFacts{
		ExpectedPhysicalSize: expectedPhysicalSize,
		ExpectedPaddingBytes: uint16(expectedPaddingBytes),
		HasExactPhysicalSize: uint64(physicalSize) == expectedPhysicalSize,
	}

	if uint64(physicalSize) >= expectedPhysicalSize {
		tail.HasZeroPadding = true
		if expectedPaddingBytes > 0 {
			var padding [squashFSPhysicalAlign - 1]byte
			checked := padding[:int(expectedPaddingBytes)]
			if err := readSquashFSAt(reader, checked, int64(superblock.BytesUsed)); err != nil {
				return squashFSFacts{}, fmt.Errorf("read SquashFS padding: %w", err)
			}
			for _, value := range checked {
				if value != 0 {
					tail.HasZeroPadding = false
					break
				}
			}
		}
	}

	return squashFSFacts{
		Superblock:   superblock,
		PhysicalSize: uint64(physicalSize),
		Tail:         tail,
	}, nil
}

func roundUpSquashFSSize(value, alignment uint64) (uint64, bool) {
	if alignment == 0 {
		return 0, false
	}
	remainder := value % alignment
	if remainder == 0 {
		return value, true
	}
	increment := alignment - remainder
	if value > ^uint64(0)-increment {
		return 0, false
	}
	return value + increment, true
}

func readSquashFSAt(reader io.ReaderAt, destination []byte, offset int64) error {
	count, err := reader.ReadAt(destination, offset)
	if count == len(destination) && (err == nil || err == io.EOF) {
		return nil
	}
	if err == nil {
		err = io.ErrUnexpectedEOF
	}
	return &artifactInfrastructureError{
		cause: fmt.Errorf(
			"read %d bytes at offset %d: got %d: %w",
			len(destination),
			offset,
			count,
			err,
		),
	}
}
