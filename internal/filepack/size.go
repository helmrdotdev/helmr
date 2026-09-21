package filepack

import (
	"encoding/json"
	"errors"

	"github.com/klauspost/compress/zstd"
)

// PackedSizeLimit bounds the current encoder's output for a stable source,
// including a completely allocated, incompressible file. It is not a decoder
// admission limit and must not be estimated from sparse allocation alone.
func PackedSizeLimit(logicalBytes int64, role string) (int64, error) {
	if logicalBytes < 0 {
		return 0, errors.New("invalid filepack logical size")
	}
	payload, err := json.Marshal(filepackHeader{
		Version: filepackVersion, Role: role, LogicalSize: logicalBytes,
		ChunkSize: filepackChunkSize, Codec: filepackCodecZstd,
	})
	if err != nil {
		return 0, err
	}
	if len(payload) > maxFilepackHeader {
		return 0, errors.New("filepack header exceeds limit")
	}
	encoder, err := newFilepackEncoder()
	if err != nil {
		return 0, err
	}
	defer encoder.Close()
	limit := int64(len(filepackMagic) + 4 + len(payload) + 1)
	full := logicalBytes / filepackChunkSize
	perChunk := int64(filepackDataHeaderSize + encoder.MaxEncodedSize(int(filepackChunkSize)))
	if full > (maxInt64-limit)/perChunk {
		return 0, errors.New("filepack encoded size overflow")
	}
	limit += full * perChunk
	if tail := logicalBytes % filepackChunkSize; tail != 0 {
		last := int64(filepackDataHeaderSize + encoder.MaxEncodedSize(int(tail)))
		if limit > maxInt64-last {
			return 0, errors.New("filepack encoded size overflow")
		}
		limit += last
	}
	return limit, nil
}

func newFilepackEncoder() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
}
