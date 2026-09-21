//go:build linux

package filepack

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

func Pack(ctx context.Context, sourcePath string, targetPath string, role string) (Stats, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return Stats{}, err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Stats{}, err
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
	stats, err := PackTo(ctx, source, target, role)
	if err != nil {
		return Stats{}, err
	}
	if err := target.Close(); err != nil {
		targetClosed = true
		return Stats{}, err
	}
	targetClosed = true
	cleanupTarget = false
	return stats, nil
}

// PackTo writes an exclusively owned, stable source into an encoded stream.
func PackTo(ctx context.Context, source *os.File, target io.Writer, role string) (Stats, error) {
	info, err := source.Stat()
	if err != nil {
		return Stats{}, err
	}
	stats := Stats{LogicalBytes: info.Size()}
	if err := writeFilepackHeader(target, filepackHeader{
		Version:     filepackVersion,
		Role:        role,
		LogicalSize: info.Size(),
		ChunkSize:   filepackChunkSize,
		Codec:       filepackCodecZstd,
	}); err != nil {
		return Stats{}, err
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		return Stats{}, err
	}
	defer encoder.Close()
	if err := writeFilepackData(ctx, source, target, encoder, &stats, info.Size()); err != nil {
		return Stats{}, err
	}
	if _, err := target.Write([]byte{filepackRecordEnd}); err != nil {
		return Stats{}, err
	}
	return stats, nil
}

func writeFilepackData(ctx context.Context, source *os.File, target io.Writer, encoder *zstd.Encoder, stats *Stats, logicalSize int64) error {
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
			return scanAndWriteFilepackRange(ctx, source, target, encoder, stats, offset, logicalSize)
		}
		if dataStart >= dataEnd {
			offset = nextOffset
			continue
		}
		// Physical extent boundaries vary across filesystems and allocation history.
		// Use them only to skip holes; encoded chunks always use logical boundaries.
		start := dataStart - dataStart%filepackChunkSize
		end := dataEnd
		if remainder := end % filepackChunkSize; remainder != 0 {
			end += min(filepackChunkSize-remainder, logicalSize-end)
		}
		if err := scanAndWriteFilepackRange(ctx, source, target, encoder, stats, start, end); err != nil {
			return err
		}
		// The final chunk may include another extent. Do not emit it twice.
		offset = end
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

func scanAndWriteFilepackRange(ctx context.Context, source *os.File, target io.Writer, encoder *zstd.Encoder, stats *Stats, start int64, end int64) error {
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
			if stats != nil {
				stats.EncodedChunks++
			}
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
		return errors.New("invalid Firecracker filepack raw chunk size")
	}
	if len(compressed) == 0 || len(compressed) > maxFilepackChunk {
		return errors.New("invalid Firecracker filepack compressed chunk size")
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
