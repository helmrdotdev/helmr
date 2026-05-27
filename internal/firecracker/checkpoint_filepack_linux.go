//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const (
	filepackMagic       = "helmr-firecracker-filepack-v0\n"
	filepackVersion     = 0
	filepackChunkSize   = int64(4 << 20)
	filepackRecordData  = byte(1)
	filepackRecordEnd   = byte(255)
	maxFilepackHeader   = 1 << 20
	maxFilepackChunk    = 64 << 20
	filepackCodecZstd   = "zstd"
	filepackScratchRole = "scratch-disk"
	filepackMemoryRole  = "memory"
	maxInt64            = int64(1<<63 - 1)
)

type filepackHeader struct {
	Version     int    `json:"version"`
	Role        string `json:"role"`
	LogicalSize int64  `json:"logical_size"`
	ChunkSize   int64  `json:"chunk_size"`
	Codec       string `json:"codec"`
}

func packRuntimeFile(ctx context.Context, sourcePath string, targetPath string, role string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	targetClosed := false
	cleanupTarget := true
	defer func() {
		if !targetClosed {
			_ = target.Close()
		}
		if cleanupTarget {
			_ = os.Remove(targetPath)
		}
	}()
	if err := writeFilepackHeader(target, filepackHeader{
		Version:     filepackVersion,
		Role:        role,
		LogicalSize: info.Size(),
		ChunkSize:   filepackChunkSize,
		Codec:       filepackCodecZstd,
	}); err != nil {
		return err
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return err
	}
	defer encoder.Close()
	if err := writeFilepackData(ctx, source, target, encoder, info.Size()); err != nil {
		return err
	}
	if _, err := target.Write([]byte{filepackRecordEnd}); err != nil {
		return err
	}
	if err := target.Close(); err != nil {
		targetClosed = true
		return err
	}
	targetClosed = true
	cleanupTarget = false
	return nil
}

func unpackRuntimeFile(ctx context.Context, sourcePath string, targetPath string, expectedRole string, expectedLogicalSize int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	header, err := readFilepackHeader(source)
	if err != nil {
		return err
	}
	if err := validateFilepackHeader(header, expectedRole); err != nil {
		return err
	}
	if expectedLogicalSize < 0 {
		return errors.New("expected firecracker filepack logical size must be non-negative")
	}
	if header.LogicalSize != expectedLogicalSize {
		return fmt.Errorf("firecracker filepack logical size %d does not match expected %d", header.LogicalSize, expectedLogicalSize)
	}
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	targetClosed := false
	cleanupTarget := true
	defer func() {
		if !targetClosed {
			_ = target.Close()
		}
		if cleanupTarget {
			_ = os.Remove(targetPath)
		}
	}()
	if err := target.Truncate(header.LogicalSize); err != nil {
		return err
	}
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		return err
	}
	defer decoder.Close()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var recordType [1]byte
		if _, err := io.ReadFull(source, recordType[:]); err != nil {
			return err
		}
		switch recordType[0] {
		case filepackRecordEnd:
			if err := target.Close(); err != nil {
				targetClosed = true
				return err
			}
			targetClosed = true
			cleanupTarget = false
			return nil
		case filepackRecordData:
			if err := readFilepackDataRecord(source, target, decoder, header.LogicalSize); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported firecracker filepack record type %d", recordType[0])
		}
	}
}

func writeFilepackHeader(w io.Writer, header filepackHeader) error {
	payload, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(payload) > maxFilepackHeader {
		return errors.New("firecracker filepack header is too large")
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
		return filepackHeader{}, errors.New("unsupported firecracker filepack format")
	}
	var encoded [4]byte
	if _, err := io.ReadFull(r, encoded[:]); err != nil {
		return filepackHeader{}, err
	}
	size := binary.BigEndian.Uint32(encoded[:])
	if size == 0 || size > maxFilepackHeader {
		return filepackHeader{}, errors.New("invalid firecracker filepack header size")
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
		return fmt.Errorf("unsupported firecracker filepack version %d", header.Version)
	}
	if header.Role != expectedRole {
		return fmt.Errorf("firecracker filepack role %q does not match %q", header.Role, expectedRole)
	}
	if header.LogicalSize < 0 {
		return errors.New("firecracker filepack logical size must be non-negative")
	}
	if header.ChunkSize <= 0 || header.ChunkSize > maxFilepackChunk {
		return errors.New("firecracker filepack chunk size is invalid")
	}
	if header.Codec != filepackCodecZstd {
		return fmt.Errorf("unsupported firecracker filepack codec %q", header.Codec)
	}
	return nil
}

func writeFilepackData(ctx context.Context, source *os.File, target io.Writer, encoder *zstd.Encoder, logicalSize int64) error {
	offset := int64(0)
	for offset < logicalSize {
		if err := ctx.Err(); err != nil {
			return err
		}
		dataStart, dataEnd, nextOffset, sparse, err := nextDataRange(source, offset, logicalSize)
		if err != nil {
			return err
		}
		if !sparse {
			return scanAndWriteFilepackRange(ctx, source, target, encoder, offset, logicalSize)
		}
		if dataStart >= dataEnd {
			offset = nextOffset
			continue
		}
		if err := scanAndWriteFilepackRange(ctx, source, target, encoder, dataStart, dataEnd); err != nil {
			return err
		}
		offset = nextOffset
	}
	return nil
}

func nextDataRange(file *os.File, offset int64, logicalSize int64) (int64, int64, int64, bool, error) {
	dataStart, err := unix.Seek(int(file.Fd()), offset, unix.SEEK_DATA)
	if err != nil {
		if errors.Is(err, unix.ENXIO) {
			return logicalSize, logicalSize, logicalSize, true, nil
		}
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
			return 0, 0, 0, false, nil
		}
		return 0, 0, 0, true, err
	}
	if dataStart >= logicalSize {
		return logicalSize, logicalSize, logicalSize, true, nil
	}
	holeStart, err := unix.Seek(int(file.Fd()), dataStart, unix.SEEK_HOLE)
	if err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTSUP) {
			return 0, 0, 0, false, nil
		}
		return 0, 0, 0, true, err
	}
	if holeStart > logicalSize {
		holeStart = logicalSize
	}
	return dataStart, holeStart, holeStart, true, nil
}

func scanAndWriteFilepackRange(ctx context.Context, source *os.File, target io.Writer, encoder *zstd.Encoder, start int64, end int64) error {
	buffer := make([]byte, int(filepackChunkSize))
	for offset := start; offset < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := end - offset
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		read := buffer[:n]
		if err := readFullAt(source, read, offset); err != nil {
			return err
		}
		if !allZero(read) {
			compressed := encoder.EncodeAll(read, nil)
			if err := writeFilepackDataRecord(target, offset, int(n), compressed); err != nil {
				return err
			}
		}
		offset += n
	}
	return nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func writeFilepackDataRecord(w io.Writer, offset int64, rawSize int, compressed []byte) error {
	if rawSize <= 0 || rawSize > maxFilepackChunk {
		return errors.New("invalid firecracker filepack raw chunk size")
	}
	if len(compressed) == 0 || len(compressed) > maxFilepackChunk {
		return errors.New("invalid firecracker filepack compressed chunk size")
	}
	var header [21]byte
	header[0] = filepackRecordData
	binary.BigEndian.PutUint64(header[1:9], uint64(offset))
	binary.BigEndian.PutUint32(header[9:13], uint32(rawSize))
	binary.BigEndian.PutUint64(header[13:21], uint64(len(compressed)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	_, err := w.Write(compressed)
	return err
}

func readFilepackDataRecord(r io.Reader, target *os.File, decoder *zstd.Decoder, logicalSize int64) error {
	var header [20]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return err
	}
	rawOffset := binary.BigEndian.Uint64(header[:8])
	if rawOffset > uint64(maxInt64) {
		return errors.New("invalid firecracker filepack data offset")
	}
	offset := int64(rawOffset)
	rawSize := int64(binary.BigEndian.Uint32(header[8:12]))
	compressedSize := int64(binary.BigEndian.Uint64(header[12:20]))
	if logicalSize < 0 || offset < 0 || offset > logicalSize || rawSize <= 0 || rawSize > maxFilepackChunk || rawSize > logicalSize-offset || compressedSize <= 0 || compressedSize > maxFilepackChunk {
		return errors.New("invalid firecracker filepack data record")
	}
	compressed := make([]byte, compressedSize)
	if _, err := io.ReadFull(r, compressed); err != nil {
		return err
	}
	decoded, err := decoder.DecodeAll(compressed, nil)
	if err != nil {
		return err
	}
	if int64(len(decoded)) != rawSize {
		return errors.New("firecracker filepack decoded chunk size mismatch")
	}
	_, err = target.WriteAt(decoded, offset)
	return err
}

func readFullAt(file *os.File, data []byte, offset int64) error {
	n, err := file.ReadAt(data, offset)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) && n == len(data) {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
