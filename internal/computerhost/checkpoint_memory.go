package computerhost

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"sync/atomic"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/klauspost/compress/zstd"
)

const (
	memoryMagic          = "helmr-memory-v1\n"
	memoryChunkSize      = vm.MemoryChunkBytes
	memoryHeaderSize     = len(memoryMagic) + keyIDSize + saltSize + 16
	memoryIndexEntrySize = 16
	memoryReadTimeout    = 15 * time.Second
)

// The logical size is allocation authority, not a size supplied by ciphertext.
func memoryGeometry(size int64) (int64, int64, error) {
	if size <= 0 || size%4096 != 0 {
		return 0, 0, errors.New("memory size must be positive and page aligned")
	}
	count := (size-1)/memoryChunkSize + 1
	if count > (math.MaxInt64-int64(memoryHeaderSize)-size-tagSize)/(memoryIndexEntrySize+tagSize) {
		return 0, 0, errors.New("memory object size overflow")
	}
	return count, size + int64(memoryHeaderSize) + count*(memoryIndexEntrySize+tagSize) + tagSize, nil
}

func (c *CheckpointEncryptor) memoryCipher(header []byte, purpose string) (cipher.AEAD, error) {
	if c == nil || len(c.key) == 0 {
		return nil, errors.New("checkpoint encryptor is required")
	}
	info := append([]byte(memoryMagic), []byte(purpose)...)
	key, err := hkdf.Key(sha256.New, c.key, header[len(memoryMagic)+keyIDSize:len(memoryMagic)+keyIDSize+saltSize], string(info), 32)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func memoryAAD(header []byte, ordinal uint64, logical int64) []byte {
	result := append([]byte(nil), header...)
	result = binary.BigEndian.AppendUint64(result, ordinal)
	return binary.BigEndian.AppendUint64(result, uint64(logical))
}

// encryptMemory streams raw RAM once. The tail index and every nonzero chunk
// are independently authenticated; the caller registers the returned header pin.
func (c *CheckpointEncryptor) encryptMemory(ctx context.Context, input io.Reader, output io.Writer, purpose string, size int64) (string, error) {
	count, _, err := memoryGeometry(size)
	if err != nil {
		return "", err
	}
	if c == nil {
		return "", errors.New("checkpoint encryptor is required")
	}
	header := make([]byte, memoryHeaderSize)
	copy(header, memoryMagic)
	copy(header[len(memoryMagic):], c.keyID[:])
	saltEnd := len(memoryMagic) + keyIDSize + saltSize
	if _, err = io.ReadFull(c.rand, header[len(memoryMagic)+keyIDSize:saltEnd]); err != nil {
		return "", err
	}
	binary.BigEndian.PutUint64(header[saltEnd:], uint64(size))
	binary.BigEndian.PutUint64(header[saltEnd+8:], uint64(count*memoryIndexEntrySize+tagSize))
	aead, err := c.memoryCipher(header, purpose)
	if err != nil {
		return "", err
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(memoryChunkSize))
	if err != nil {
		return "", err
	}
	defer encoder.Close()
	if _, err = output.Write(header); err != nil {
		return "", err
	}
	index := make([]byte, count*memoryIndexEntrySize)
	raw := make([]byte, memoryChunkSize)
	zero := make([]byte, memoryChunkSize)
	offset := int64(memoryHeaderSize)
	for i := int64(0); i < count; i++ {
		if err = ctx.Err(); err != nil {
			return "", err
		}
		logical := min(int64(memoryChunkSize), size-i*memoryChunkSize)
		chunk := raw[:logical]
		if _, err = io.ReadFull(input, chunk); err != nil {
			return "", err
		}
		entry := index[i*memoryIndexEntrySize : (i+1)*memoryIndexEntrySize]
		binary.BigEndian.PutUint64(entry, uint64(offset))
		if bytes.Equal(chunk, zero[:logical]) {
			continue
		}
		encoded := encoder.EncodeAll(chunk, nil)
		kind := byte(2)
		if len(encoded) >= len(chunk) {
			encoded = chunk
			kind = 1
		}
		sealed := aead.Seal(nil, checkpointNonce(uint64(i+1)), encoded, memoryAAD(header, uint64(i+1), logical))
		binary.BigEndian.PutUint32(entry[8:], uint32(len(sealed)))
		entry[12] = kind
		if _, err = output.Write(sealed); err != nil {
			return "", err
		}
		offset += int64(len(sealed))
	}
	var extra [1]byte
	n, err := io.ReadFull(input, extra[:])
	if n != 0 || err != io.EOF {
		return "", errors.New("raw memory exceeds admitted size")
	}
	sealed := aead.Seal(nil, checkpointNonce(0), index, memoryAAD(header, 0, size))
	if _, err = output.Write(sealed); err != nil {
		return "", err
	}
	return sha256sum.DigestBytes(header), nil
}

type checkpointMemorySource struct {
	ranges           blockformat.RangeSource
	descriptor       cas.Descriptor
	size             int64
	header, index    []byte
	aead             cipher.AEAD
	rangeRequests    atomic.Int64
	ciphertextBytes  atomic.Int64
	decodeNanos      atomic.Int64
	rangeHeaderNanos atomic.Int64
	rangeBodyNanos   atomic.Int64
	rangeActive      atomic.Int64
	rangePeak        atomic.Int64
}

func openCheckpointMemory(ctx context.Context, ranges blockformat.RangeSource, c *CheckpointEncryptor, descriptor cas.Descriptor, headerDigest, purpose string, size int64) (*checkpointMemorySource, error) {
	count, bound, err := memoryGeometry(size)
	if err != nil {
		return nil, err
	}
	if ranges == nil || c == nil {
		return nil, errors.New("memory restore source is required")
	}
	if err = cas.ValidateDescriptor(descriptor); err != nil {
		return nil, invalidMemory(err)
	}
	indexSize := count*memoryIndexEntrySize + tagSize
	if !sha256sum.ValidDigest(headerDigest) || descriptor.SizeBytes < int64(memoryHeaderSize)+indexSize || descriptor.SizeBytes > bound {
		return nil, invalidMemory(errors.New("invalid memory object bounds or header pin"))
	}
	s := &checkpointMemorySource{ranges: ranges, descriptor: descriptor, size: size}
	header, err := s.readRange(ctx, 0, int64(memoryHeaderSize))
	if err != nil {
		return nil, err
	}
	if sha256sum.DigestBytes(header) != headerDigest || string(header[:len(memoryMagic)]) != memoryMagic {
		return nil, invalidMemory(errors.New("memory header does not match its registered pin"))
	}
	if !bytes.Equal(header[len(memoryMagic):len(memoryMagic)+keyIDSize], c.keyID[:]) {
		return nil, ErrCheckpointKeyUnavailable
	}
	saltEnd := len(memoryMagic) + keyIDSize + saltSize
	if binary.BigEndian.Uint64(header[saltEnd:]) != uint64(size) || binary.BigEndian.Uint64(header[saltEnd+8:]) != uint64(indexSize) {
		return nil, invalidMemory(errors.New("memory geometry differs from allocation"))
	}
	s.header = header
	s.aead, err = c.memoryCipher(header, purpose)
	if err != nil {
		return nil, err
	}
	tail := descriptor.SizeBytes - indexSize
	sealed, err := s.readRange(ctx, tail, indexSize)
	if err != nil {
		return nil, err
	}
	s.index, err = s.aead.Open(sealed[:0], checkpointNonce(0), sealed, memoryAAD(header, 0, size))
	if err != nil {
		return nil, invalidMemory(fmt.Errorf("authenticate memory index: %w", err))
	}
	offset := int64(memoryHeaderSize)
	for i := int64(0); i < count; i++ {
		entry := s.index[i*memoryIndexEntrySize : (i+1)*memoryIndexEntrySize]
		length := int64(binary.BigEndian.Uint32(entry[8:]))
		kind := entry[12]
		logical := min(int64(memoryChunkSize), size-i*memoryChunkSize)
		if binary.BigEndian.Uint64(entry) != uint64(offset) || entry[13] != 0 || entry[14] != 0 || entry[15] != 0 || length > tail-offset {
			return nil, invalidMemory(errors.New("invalid memory index range"))
		}
		switch kind {
		case 0:
			if length != 0 {
				return nil, invalidMemory(errors.New("invalid zero memory chunk"))
			}
		case 1:
			if length != logical+tagSize {
				return nil, invalidMemory(errors.New("invalid raw memory chunk"))
			}
		case 2:
			if length <= tagSize || length >= logical+tagSize {
				return nil, invalidMemory(errors.New("invalid compressed memory chunk"))
			}
		default:
			return nil, invalidMemory(errors.New("unsupported memory chunk encoding"))
		}
		offset += length
	}
	if offset != tail {
		return nil, invalidMemory(errors.New("memory payload has unindexed bytes"))
	}
	return s, nil
}

func (s *checkpointMemorySource) Size() int64 { return s.size }
func (s *checkpointMemorySource) MemoryReadStats() vm.MemoryReadStats {
	return vm.MemoryReadStats{RangeHeaderMs: s.rangeHeaderNanos.Load() / int64(time.Millisecond), RangeBodyMs: s.rangeBodyNanos.Load() / int64(time.Millisecond), RangeActive: s.rangeActive.Load(), RangePeak: s.rangePeak.Load(), RangeRequests: s.rangeRequests.Load(), CiphertextBytes: s.ciphertextBytes.Load(), DecodeDurationMs: vm.RuntimeDurationMilliseconds(time.Duration(s.decodeNanos.Load()))}
}

// One bounded logical window maps to one contiguous ciphertext range. Every
// persisted chunk still authenticates independently before the pager uses it.
func (s *checkpointMemorySource) ReadAt(ctx context.Context, dst []byte, offset int64) (int, error) {
	if offset < 0 || offset >= s.size || len(dst) == 0 || int64(len(dst)) > s.size-offset || offset/vm.MemoryReadBytes != (offset+int64(len(dst))-1)/vm.MemoryReadBytes {
		return 0, errors.New("invalid memory window read")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	first, last := offset/memoryChunkSize, (offset+int64(len(dst))-1)/memoryChunkSize
	firstEntry := s.index[first*memoryIndexEntrySize : (first+1)*memoryIndexEntrySize]
	lastEntry := s.index[last*memoryIndexEntrySize : (last+1)*memoryIndexEntrySize]
	start := int64(binary.BigEndian.Uint64(firstEntry))
	end := int64(binary.BigEndian.Uint64(lastEntry)) + int64(binary.BigEndian.Uint32(lastEntry[8:]))
	var sealed []byte
	if end > start {
		var err error
		sealed, err = s.readRange(ctx, start, end-start)
		if err != nil {
			return 0, err
		}
		defer clear(sealed)
	}

	started := time.Now()
	defer func() { s.decodeNanos.Add(int64(time.Since(started))) }()
	var decoder *zstd.Decoder
	defer func() {
		if decoder != nil {
			decoder.Close()
		}
	}()
	var scratch []byte
	defer func() { clear(scratch[:cap(scratch)]) }()
	written := 0
	for ordinal := first; ordinal <= last; ordinal++ {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		entry := s.index[ordinal*memoryIndexEntrySize : (ordinal+1)*memoryIndexEntrySize]
		logical := min(int64(memoryChunkSize), s.size-ordinal*memoryChunkSize)
		chunkOffset := max(offset-ordinal*memoryChunkSize, 0)
		count := min(logical-chunkOffset, int64(len(dst)-written))
		target := dst[written : written+int(count)]
		if entry[12] == 0 {
			clear(target)
		} else {
			at := int64(binary.BigEndian.Uint64(entry)) - start
			length := int64(binary.BigEndian.Uint32(entry[8:]))
			if entry[12] == 2 && decoder == nil {
				scratch = make([]byte, 0, memoryChunkSize)
				var err error
				decoder, err = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(memoryChunkSize*2), zstd.WithDecoderMaxWindow(memoryChunkSize), zstd.WithDecodeAllCapLimit(true))
				if err != nil {
					return written, err
				}
			}
			if err := s.decodeChunk(target, sealed[at:at+length], ordinal, logical, chunkOffset, entry[12], decoder, scratch); err != nil {
				return written, err
			}
		}
		written += int(count)
	}
	return written, nil
}
func (s *checkpointMemorySource) decodeChunk(dst, sealed []byte, ordinal, logical, offset int64, kind byte, decoder *zstd.Decoder, scratch []byte) error {
	opened, err := s.aead.Open(sealed[:0], checkpointNonce(uint64(ordinal+1)), sealed, memoryAAD(s.header, uint64(ordinal+1), logical))
	if err != nil {
		return invalidMemory(fmt.Errorf("authenticate memory chunk %d: %w", ordinal, err))
	}
	defer clear(opened)
	if kind == 2 {
		decoded, err := decoder.DecodeAll(opened, scratch[:0])
		if err != nil {
			return invalidMemory(err)
		}
		defer clear(decoded)
		opened = decoded
	}
	if int64(len(opened)) != logical {
		return invalidMemory(errors.New("decoded memory length differs from allocation"))
	}
	copy(dst, opened[offset:])
	return nil
}

func (s *checkpointMemorySource) readRange(ctx context.Context, offset, length int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, memoryReadTimeout)
	defer cancel()
	for delay := 50 * time.Millisecond; ; delay = min(delay*2, time.Second) {
		data, err := s.readRangeOnce(ctx, offset, length)
		if err == nil {
			return data, nil
		}
		var network net.Error
		if !errors.Is(err, cas.ErrUnavailable) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.As(err, &network) {
			return nil, err
		}
		if err = sleepWithContext(ctx, delay); err != nil {
			return nil, err
		}
	}
}
func (s *checkpointMemorySource) readRangeOnce(ctx context.Context, offset, length int64) ([]byte, error) {
	s.rangeRequests.Add(1)
	active := s.rangeActive.Add(1)
	defer s.rangeActive.Add(-1)
	for peak := s.rangePeak.Load(); active > peak; peak = s.rangePeak.Load() {
		if s.rangePeak.CompareAndSwap(peak, active) {
			break
		}
	}
	headerStarted := time.Now()
	body, err := s.ranges.GetRange(ctx, s.descriptor.Digest, s.descriptor.SizeBytes, offset, length)
	s.rangeHeaderNanos.Add(int64(time.Since(headerStarted)))
	if err != nil {
		classified := checkpointObjectReadError(err)
		if errors.Is(classified, errInvalidCheckpoint) {
			return nil, invalidMemory(classified)
		}
		return nil, classified
	}
	bodyStarted := time.Now()
	defer func() { s.rangeBodyNanos.Add(int64(time.Since(bodyStarted))) }()
	data := make([]byte, length)
	n, readErr := io.ReadFull(body, data)
	s.ciphertextBytes.Add(int64(n))
	if readErr == io.EOF {
		readErr = io.ErrUnexpectedEOF
	}
	if readErr == nil {
		var extra [1]byte
		n, err := io.ReadFull(body, extra[:])
		if n != 0 {
			readErr = invalidMemory(errors.New("memory range exceeds descriptor"))
		} else if err != io.EOF {
			readErr = err
		}
	}
	closeErr := body.Close()
	if err = errors.Join(readErr, closeErr); err != nil {
		classified := checkpointObjectReadError(err)
		if errors.Is(classified, errInvalidCheckpoint) {
			return nil, invalidMemory(classified)
		}
		return nil, classified
	}
	return data, nil
}

func invalidMemory(err error) error {
	return errors.Join(invalidCheckpoint(err), vm.ErrInvalidCheckpointContent)
}
