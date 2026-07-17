package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestSquashFSMetadataDecoderReadsBlocks(t *testing.T) {
	tests := []struct {
		name       string
		data       []byte
		compressed bool
	}{
		{name: "one uncompressed byte", data: []byte{0x7f}},
		{
			name:       "compressed block",
			data:       bytes.Repeat([]byte("metadata"), 300),
			compressed: true,
		},
		{
			name: "full uncompressed block",
			data: bytes.Repeat([]byte{0x5a}, squashFSMetadataBlockSize),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			encoded := squashFSTestMetadataBlock(t, test.data, test.compressed)
			decoder := newSquashFSTestMetadataDecoder(t, encoded)
			block, err := decoder.readBlock(
				context.Background(),
				squashFSRegion{end: uint64(len(encoded))},
				0,
				uint64(len(test.data)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(block.data, test.data) {
				t.Fatalf("decoded data differs")
			}
			if block.end != uint64(len(encoded)) {
				t.Fatalf("block end = %d, want %d", block.end, len(encoded))
			}
		})
	}
}

func TestSquashFSMetadataCursorReadsAcrossBlocks(t *testing.T) {
	first := squashFSTestMetadataBlock(t, []byte("abc"), false)
	second := squashFSTestMetadataBlock(t, []byte("defgh"), true)
	image := append(first, second...)
	decoder := newSquashFSTestMetadataDecoder(t, image)
	cursor, err := newSquashFSMetadataCursor(
		context.Background(),
		decoder,
		squashFSRegion{end: uint64(len(image))},
		0,
		1,
	)
	if err != nil {
		t.Fatal(err)
	}

	var prefix [2]byte
	if err := cursor.readFull(prefix[:]); err != nil {
		t.Fatal(err)
	}
	if string(prefix[:]) != "bc" {
		t.Fatalf("prefix = %q", prefix[:])
	}
	var remainder [5]byte
	if err := cursor.readFull(remainder[:]); err != nil {
		t.Fatal(err)
	}
	if string(remainder[:]) != "defgh" {
		t.Fatalf("remainder = %q", remainder[:])
	}
}

func TestSquashFSMetadataCursorRejectsPositionAtBlockEnd(t *testing.T) {
	image := squashFSTestMetadataBlock(t, []byte("abc"), false)
	decoder := newSquashFSTestMetadataDecoder(t, image)
	_, err := newSquashFSMetadataCursor(
		context.Background(),
		decoder,
		squashFSRegion{end: uint64(len(image))},
		0,
		3,
	)
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T, want artifactContentError: %v", err, err)
	}
}

func TestReadSquashFSIDTable(t *testing.T) {
	tests := []struct {
		name       string
		count      int
		compressed bool
	}{
		{name: "one ID", count: 1},
		{name: "one full metadata block", count: 2048, compressed: true},
		{name: "two metadata blocks", count: 2049},
		{name: "maximum ID count", count: 65535},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values := make([]uint32, test.count)
			for index := range values {
				if test.compressed {
					values[index] = uint32(index % 4)
				} else {
					values[index] = uint32(index) ^ 0xa1b2c3d4
				}
			}
			image, dataStart, indexStart := squashFSTestIDTable(
				t,
				values,
				test.compressed,
			)
			decoder := newSquashFSTestMetadataDecoder(t, image)
			decoded, err := readSquashFSIDTable(
				context.Background(),
				decoder,
				dataStart,
				indexStart,
				uint64(len(image)),
				uint16(len(values)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(decoded) != len(values) {
				t.Fatalf("decoded count = %d, want %d", len(decoded), len(values))
			}
			for index := range values {
				if decoded[index] != values[index] {
					t.Fatalf(
						"decoded[%d] = %#x, want %#x",
						index,
						decoded[index],
						values[index],
					)
				}
			}
		})
	}
}

func TestSquashFSMetadataDecoderRejectsInvalidContent(t *testing.T) {
	validCompressed := squashFSTestMetadataBlock(t, []byte("content"), true)
	validFrame := validCompressed[2:]
	tests := []struct {
		name     string
		image    []byte
		region   squashFSRegion
		expected uint64
	}{
		{
			name:   "truncated header",
			image:  []byte{1},
			region: squashFSRegion{end: 1},
		},
		{
			name:   "zero stored size",
			image:  []byte{0, 0},
			region: squashFSRegion{end: 2},
		},
		{
			name:   "stored size above limit",
			image:  []byte{0x01, 0x20},
			region: squashFSRegion{end: 2},
		},
		{
			name:   "payload crosses region",
			image:  []byte{2, 0, 1},
			region: squashFSRegion{end: 3},
		},
		{
			name:     "uncompressed expected-size mismatch",
			image:    squashFSTestMetadataBlock(t, []byte("ab"), false),
			region:   squashFSRegion{end: 4},
			expected: 1,
		},
		{
			name:     "compressed expected-size mismatch",
			image:    validCompressed,
			region:   squashFSRegion{end: uint64(len(validCompressed))},
			expected: 6,
		},
		{
			name:   "skippable frame",
			image:  squashFSTestEncodedMetadata([]byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}),
			region: squashFSRegion{end: 10},
		},
		{
			name:   "concatenated frames",
			image:  squashFSTestEncodedMetadata(append(append([]byte{}, validFrame...), validFrame...)),
			region: squashFSRegion{end: uint64(2 + len(validFrame)*2)},
		},
		{
			name:   "trailing frame byte",
			image:  squashFSTestEncodedMetadata(append(append([]byte{}, validFrame...), 0)),
			region: squashFSRegion{end: uint64(3 + len(validFrame))},
		},
		{
			name: "dictionary ID",
			image: squashFSTestEncodedMetadata(
				[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x21, 1, 0, 1, 0, 0},
			),
			region: squashFSRegion{end: 12},
		},
		{
			name: "window above limit",
			image: squashFSTestEncodedMetadata(
				[]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 64, 1, 0, 0},
			),
			region: squashFSRegion{end: 11},
		},
		{
			name: "reserved Zstandard block",
			image: squashFSTestEncodedMetadata(
				[]byte{0x28, 0xb5, 0x2f, 0xfd, 0x20, 0, 7, 0, 0},
			),
			region: squashFSRegion{end: 11},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := newSquashFSTestMetadataDecoder(t, test.image)
			_, err := decoder.readBlock(
				context.Background(),
				test.region,
				0,
				test.expected,
			)
			if err == nil {
				t.Fatal("invalid content was accepted")
			}
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError: %v", err, err)
			}
		})
	}
}

func TestSquashFSMetadataDecoderClassifiesBackingFailureAsInfrastructure(t *testing.T) {
	reader := squashFSReaderAtFunc(func(destination []byte, offset int64) (int, error) {
		if offset == 0 {
			copy(destination, []byte{1, 0x80})
			return len(destination), nil
		}
		return 0, io.ErrUnexpectedEOF
	})
	decoder, err := newSquashFSMetadataDecoder(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)
	_, err = decoder.readBlock(
		context.Background(),
		squashFSRegion{end: 3},
		0,
		1,
	)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError: %v", err, err)
	}
}

func TestSquashFSMetadataDecoderHonorsContext(t *testing.T) {
	encoded := squashFSTestMetadataBlock(t, []byte("content"), true)
	decoder := newSquashFSTestMetadataDecoder(t, encoded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := decoder.readBlock(
		ctx,
		squashFSRegion{end: uint64(len(encoded))},
		0,
		0,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError", err)
	}
}

func TestReadSquashFSIDTableRejectsInvalidSpans(t *testing.T) {
	values := make([]uint32, 2049)
	image, dataStart, indexStart := squashFSTestIDTable(t, values, false)
	tests := []struct {
		name       string
		mutate     func([]byte)
		dataStart  uint64
		indexStart uint64
		bytesUsed  uint64
	}{
		{
			name:       "index does not end at bytes used",
			dataStart:  dataStart,
			indexStart: indexStart,
			bytesUsed:  uint64(len(image)) + 1,
		},
		{
			name: "first pointer does not own data start",
			mutate: func(changed []byte) {
				binary.LittleEndian.PutUint64(changed[indexStart:indexStart+8], dataStart+1)
			},
			dataStart:  dataStart,
			indexStart: indexStart,
			bytesUsed:  uint64(len(image)),
		},
		{
			name: "gap between blocks",
			mutate: func(changed []byte) {
				second := binary.LittleEndian.Uint64(changed[indexStart+8 : indexStart+16])
				binary.LittleEndian.PutUint64(changed[indexStart+8:indexStart+16], second+1)
			},
			dataStart:  dataStart,
			indexStart: indexStart,
			bytesUsed:  uint64(len(image)),
		},
		{
			name: "backward second pointer",
			mutate: func(changed []byte) {
				binary.LittleEndian.PutUint64(changed[indexStart+8:indexStart+16], dataStart)
			},
			dataStart:  dataStart,
			indexStart: indexStart,
			bytesUsed:  uint64(len(image)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := append([]byte{}, image...)
			if test.mutate != nil {
				test.mutate(changed)
			}
			decoder := newSquashFSTestMetadataDecoder(t, changed)
			_, err := readSquashFSIDTable(
				context.Background(),
				decoder,
				test.dataStart,
				test.indexStart,
				test.bytesUsed,
				uint16(len(values)),
			)
			if err == nil {
				t.Fatal("invalid ID table was accepted")
			}
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError: %v", err, err)
			}
		})
	}
}

func TestSquashFSMetadataDecoderAcceptsFullReadWithEOF(t *testing.T) {
	image := squashFSTestMetadataBlock(t, []byte("abc"), false)
	reader := squashFSReaderAtFunc(func(destination []byte, offset int64) (int, error) {
		count, err := bytes.NewReader(image).ReadAt(destination, offset)
		if count == len(destination) {
			return count, io.EOF
		}
		return count, err
	})
	decoder, err := newSquashFSMetadataDecoder(reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)
	block, err := decoder.readBlock(
		context.Background(),
		squashFSRegion{end: uint64(len(image))},
		0,
		3,
	)
	if err != nil {
		t.Fatal(err)
	}
	if string(block.data) != "abc" {
		t.Fatalf("decoded = %q", block.data)
	}
}

func TestSquashFSMetadataDecoderRejectsNilContext(t *testing.T) {
	image := squashFSTestMetadataBlock(t, []byte("abc"), false)
	decoder := newSquashFSTestMetadataDecoder(t, image)
	_, err := decoder.readBlock(
		nil,
		squashFSRegion{end: uint64(len(image))},
		0,
		3,
	)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) ||
		!strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("error = %T %v, want nil-context infrastructure error", err, err)
	}
}

func TestSquashFSMetadataDecoderClassifiesReadAfterCloseAsInfrastructure(t *testing.T) {
	image := squashFSTestMetadataBlock(t, []byte("abc"), false)
	decoder, err := newSquashFSMetadataDecoder(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	decoder.Close()
	_, err = decoder.readBlock(
		context.Background(),
		squashFSRegion{end: uint64(len(image))},
		0,
		3,
	)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError: %v", err, err)
	}
}

func FuzzSquashFSMetadataDecoder(f *testing.F) {
	f.Add([]byte{1, 0x80, 0x7f})
	f.Add([]byte{0, 0})
	f.Add([]byte{1})
	f.Fuzz(func(t *testing.T, image []byte) {
		decoder, err := newSquashFSMetadataDecoder(bytes.NewReader(image))
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		_, err = decoder.readBlock(
			context.Background(),
			squashFSRegion{end: uint64(len(image))},
			0,
			0,
		)
		if err == nil {
			return
		}
		var contentError *artifactContentError
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &contentError) &&
			!errors.As(err, &infrastructureError) {
			t.Fatalf("untyped error %T: %v", err, err)
		}
	})
}

func newSquashFSTestMetadataDecoder(
	t *testing.T,
	image []byte,
) *squashFSMetadataDecoder {
	t.Helper()
	decoder, err := newSquashFSMetadataDecoder(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)
	return decoder
}

func squashFSTestMetadataBlock(
	t *testing.T,
	data []byte,
	compressed bool,
) []byte {
	t.Helper()
	if !compressed {
		if len(data) > squashFSMetadataBlockSize {
			t.Fatalf("test metadata size = %d", len(data))
		}
		encoded := make([]byte, 2+len(data))
		binary.LittleEndian.PutUint16(encoded[:2], uint16(len(data))|0x8000)
		copy(encoded[2:], data)
		return encoded
	}
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	return squashFSTestEncodedMetadata(writer.EncodeAll(data, nil))
}

func squashFSTestEncodedMetadata(payload []byte) []byte {
	encoded := make([]byte, 2+len(payload))
	binary.LittleEndian.PutUint16(encoded[:2], uint16(len(payload)))
	copy(encoded[2:], payload)
	return encoded
}

func squashFSTestIDTable(
	t *testing.T,
	values []uint32,
	compressed bool,
) ([]byte, uint64, uint64) {
	t.Helper()
	dataStart := uint64(96)
	image := make([]byte, dataStart)
	encodedValues := make([]byte, len(values)*4)
	for index, value := range values {
		binary.LittleEndian.PutUint32(encodedValues[index*4:index*4+4], value)
	}

	var pointers []uint64
	for len(encodedValues) > 0 {
		size := min(len(encodedValues), squashFSMetadataBlockSize)
		pointers = append(pointers, uint64(len(image)))
		block := squashFSTestMetadataBlock(t, encodedValues[:size], compressed)
		image = append(image, block...)
		encodedValues = encodedValues[size:]
	}
	indexStart := uint64(len(image))
	for _, pointer := range pointers {
		var encoded [8]byte
		binary.LittleEndian.PutUint64(encoded[:], pointer)
		image = append(image, encoded[:]...)
	}
	return image, dataStart, indexStart
}
