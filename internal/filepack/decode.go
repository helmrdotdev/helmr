package filepack

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

const (
	filepackMagic      = "helmr-firecracker-filepack-v0\n"
	filepackVersion    = 0
	filepackChunkSize  = int64(4 << 20)
	filepackRecordData = byte(1)
	filepackRecordEnd  = byte(255)
	maxFilepackHeader  = 1 << 20
	maxFilepackChunk   = 64 << 20
	filepackCodecZstd  = "zstd"
	ScratchRole        = "scratch-disk"
	MemoryRole         = "memory"
	maxInt64           = int64(1<<63 - 1)
)

type filepackHeader struct {
	Version     int    `json:"version"`
	Role        string `json:"role"`
	LogicalSize int64  `json:"logical_size"`
	ChunkSize   int64  `json:"chunk_size"`
	Codec       string `json:"codec"`
}

func Unpack(ctx context.Context, sourcePath, targetPath, expectedRole string, expectedLogicalSize int64) (Stats, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return Stats{}, err
	}
	defer source.Close()
	return UnpackFrom(ctx, source, targetPath, expectedRole, expectedLogicalSize)
}

// UnpackFrom creates the target exclusively and removes it on any decode failure.
func UnpackFrom(ctx context.Context, source io.Reader, targetPath, expectedRole string, expectedLogicalSize int64) (_ Stats, retErr error) {
	if expectedLogicalSize < 0 {
		return Stats{}, errors.New("invalid logical size")
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return Stats{}, err
	}
	defer func() {
		if retErr != nil {
			_ = target.Close()
			_ = os.Remove(targetPath)
		}
	}()
	if err := target.Truncate(expectedLogicalSize); err != nil {
		return Stats{}, err
	}
	stats, err := decodeFilepack(ctx, source, target, expectedRole, expectedLogicalSize)
	if err != nil {
		return stats, err
	}
	if err := target.Close(); err != nil {
		return stats, err
	}
	return stats, nil
}

// VerifyFrom validates all encoded records without creating a logical-size disk.
// The caller bounds input bytes, deadline and concurrent verification operations.
func VerifyFrom(ctx context.Context, source io.Reader, expectedRole string, expectedLogicalSize int64) (Stats, error) {
	return decodeFilepack(ctx, source, discardAt{}, expectedRole, expectedLogicalSize)
}

type discardAt struct{}

func (discardAt) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }

func decodeFilepack(ctx context.Context, source io.Reader, target io.WriterAt, expectedRole string, expectedLogicalSize int64) (Stats, error) {
	header, err := readFilepackHeader(source)
	if err != nil {
		return Stats{}, err
	}
	if err := validateFilepackHeader(header, expectedRole); err != nil {
		return Stats{}, err
	}
	if expectedLogicalSize < 0 {
		return Stats{}, errors.New("expected Firecracker filepack logical size must be non-negative")
	}
	if header.LogicalSize != expectedLogicalSize {
		return Stats{}, fmt.Errorf("the Firecracker filepack logical size %d does not match expected %d", header.LogicalSize, expectedLogicalSize)
	}
	stats := Stats{LogicalBytes: header.LogicalSize}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(filepackChunkSize)), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return stats, err
	}
	defer decoder.Close()
	var nextOffset int64
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		var recordType [1]byte
		if _, err := io.ReadFull(source, recordType[:]); err != nil {
			return stats, err
		}
		switch recordType[0] {
		case filepackRecordEnd:
			var trailing [1]byte
			if n, err := source.Read(trailing[:]); n != 0 || !errors.Is(err, io.EOF) {
				return stats, errors.New("filepack has trailing data or incomplete end")
			}
			return stats, nil
		case filepackRecordData:
			if err := readFilepackDataRecord(source, target, decoder, &stats, header.LogicalSize, &nextOffset); err != nil {
				return stats, err
			}
		default:
			return stats, fmt.Errorf("unsupported Firecracker filepack record type %d", recordType[0])
		}
	}
}

func writeFilepackHeader(w io.Writer, header filepackHeader) error {
	payload, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(payload) > maxFilepackHeader {
		return errors.New("the Firecracker filepack header is too large")
	}
	if _, err := io.WriteString(w, filepackMagic); err != nil {
		return err
	}
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(len(payload)))
	if _, err := w.Write(encoded[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func readFilepackHeader(r io.Reader) (filepackHeader, error) {
	prefix := make([]byte, len(filepackMagic))
	if _, err := io.ReadFull(r, prefix); err != nil {
		return filepackHeader{}, err
	}
	if string(prefix) != filepackMagic {
		return filepackHeader{}, errors.New("unsupported Firecracker filepack format")
	}
	var encoded [4]byte
	if _, err := io.ReadFull(r, encoded[:]); err != nil {
		return filepackHeader{}, err
	}
	size := binary.BigEndian.Uint32(encoded[:])
	if size == 0 || size > maxFilepackHeader {
		return filepackHeader{}, errors.New("invalid Firecracker filepack header size")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return filepackHeader{}, err
	}
	var header filepackHeader
	if err := json.Unmarshal(payload, &header); err != nil {
		return filepackHeader{}, err
	}
	return header, nil
}

func validateFilepackHeader(header filepackHeader, expectedRole string) error {
	if header.Version != filepackVersion {
		return fmt.Errorf("unsupported Firecracker filepack version %d", header.Version)
	}
	if header.Role != expectedRole {
		return fmt.Errorf("the Firecracker filepack role %q does not match %q", header.Role, expectedRole)
	}
	if header.LogicalSize < 0 {
		return errors.New("the Firecracker filepack logical size must be non-negative")
	}
	if header.ChunkSize != filepackChunkSize {
		return errors.New("the Firecracker filepack chunk size is invalid")
	}
	if header.Codec != filepackCodecZstd {
		return fmt.Errorf("unsupported Firecracker filepack codec %q", header.Codec)
	}
	return nil
}

func readFilepackDataRecord(r io.Reader, target io.WriterAt, decoder *zstd.Decoder, stats *Stats, logicalSize int64, nextOffset *int64) error {
	var header [20]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	rawOffset := binary.BigEndian.Uint64(header[:8])
	if rawOffset > uint64(maxInt64) {
		return errors.New("invalid Firecracker filepack data offset")
	}
	offset := int64(rawOffset)
	rawSize := int64(binary.BigEndian.Uint32(header[8:12]))
	compressedSize := int64(binary.BigEndian.Uint64(header[12:20]))
	if logicalSize < 0 || offset < 0 || offset > logicalSize || rawSize <= 0 || rawSize > maxFilepackChunk || rawSize > logicalSize-offset || compressedSize <= 0 || compressedSize > maxFilepackChunk {
		return errors.New("invalid Firecracker filepack data record")
	}
	// Canonical ordered chunks bound both record count and total decoded writes.
	// Check before reading or allocating attacker-controlled compressed contents.
	if offset < *nextOffset || offset%filepackChunkSize != 0 || rawSize != min(filepackChunkSize, logicalSize-offset) {
		return errors.New("filepack records must be ordered nonoverlapping logical chunks")
	}
	compressed := make([]byte, compressedSize)
	if _, err := io.ReadFull(r, compressed); err != nil {
		return err
	}
	decoded, err := decoder.DecodeAll(compressed, make([]byte, 0, rawSize))
	if err != nil {
		return err
	}
	if int64(len(decoded)) != rawSize {
		return errors.New("the Firecracker filepack decoded chunk size mismatch")
	}
	if _, err = target.WriteAt(decoded, offset); err != nil {
		return err
	}
	*nextOffset = offset + rawSize
	if stats != nil {
		stats.EncodedChunks++
		stats.UnpackWrittenBytes += rawSize
	}
	return nil
}
