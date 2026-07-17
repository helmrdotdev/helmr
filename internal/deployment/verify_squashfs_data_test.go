package deployment

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestSquashFSDataReaderStreamsMixedBlocks(t *testing.T) {
	compressed := bytes.Repeat([]byte("compressed-"), 2000)
	uncompressed := bytes.Repeat([]byte{0xa5}, squashFSDataBlockSize)
	final := []byte("final")
	image := make([]byte, squashFSSuperblockSize)
	blocks := []squashFSBlockFacts{
		squashFSTestDataBlock(t, &image, compressed, false),
		squashFSTestDataBlock(t, &image, uncompressed, true),
		{
			LogicalSize: squashFSDataBlockSize,
			Sparse:      true,
		},
		squashFSTestDataBlock(t, &image, final, true),
	}
	regular := squashFSRegularFacts{
		Size:        uint64(len(compressed) + len(uncompressed) + squashFSDataBlockSize + len(final)),
		SparseBytes: squashFSDataBlockSize,
		Fragment:    squashFSInvalidFragment,
		Blocks:      blocks,
	}
	reader := squashFSTestDataReader(
		t,
		image,
		"dir/file",
		squashFSExtendedRegularForm,
		regular,
	)

	opened, err := reader.Open(context.Background(), "dir/file")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var got bytes.Buffer
	buffer := make([]byte, 37)
	for {
		count, err := opened.Read(buffer)
		got.Write(buffer[:count])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	want := append(append([]byte(nil), compressed...), uncompressed...)
	want = append(want, make([]byte, squashFSDataBlockSize)...)
	want = append(want, final...)
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("decoded %d bytes, want %d", got.Len(), len(want))
	}
	if count, err := opened.Read(buffer); count != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after EOF = %d, %v", count, err)
	}
}

func TestSquashFSDataReaderOpensEmptyFile(t *testing.T) {
	regular := squashFSRegularFacts{
		Fragment: squashFSInvalidFragment,
	}
	reader := squashFSTestDataReader(
		t,
		make([]byte, squashFSSuperblockSize),
		"empty",
		squashFSBasicRegularForm,
		regular,
	)
	opened, err := reader.Open(context.Background(), "empty")
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if count, err := opened.Read(make([]byte, 1)); count != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("empty read = %d, %v", count, err)
	}
}

func TestSquashFSDataReaderAcceptsSparseAccountingForms(t *testing.T) {
	tests := []struct {
		name    string
		form    uint16
		regular squashFSRegularFacts
	}{
		{
			name: "basic sparse descriptor",
			form: squashFSBasicRegularForm,
			regular: squashFSRegularFacts{
				Size:     squashFSDataBlockSize,
				Fragment: squashFSInvalidFragment,
				Blocks: []squashFSBlockFacts{{
					LogicalSize: squashFSDataBlockSize,
					Sparse:      true,
				}},
			},
		},
		{
			name: "fully sparse extended convention",
			form: squashFSExtendedRegularForm,
			regular: squashFSRegularFacts{
				Size:        squashFSDataBlockSize * 2,
				SparseBytes: squashFSDataBlockSize*2 - 1,
				Fragment:    squashFSInvalidFragment,
				Blocks: []squashFSBlockFacts{
					{LogicalSize: squashFSDataBlockSize, Sparse: true},
					{LogicalSize: squashFSDataBlockSize, Sparse: true},
				},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := squashFSTestDataReader(
				t,
				make([]byte, squashFSSuperblockSize),
				"file",
				test.form,
				test.regular,
			)
			opened, err := reader.Open(context.Background(), "file")
			if err != nil {
				t.Fatal(err)
			}
			defer opened.Close()
			written, err := io.Copy(io.Discard, opened)
			if err != nil {
				t.Fatal(err)
			}
			if written != int64(test.regular.Size) {
				t.Fatalf("read %d bytes, want %d", written, test.regular.Size)
			}
		})
	}
}

func TestSquashFSDataReaderRejectsIncorrectExtendedSparseCount(t *testing.T) {
	regular := squashFSRegularFacts{
		Size:        squashFSDataBlockSize,
		SparseBytes: squashFSDataBlockSize - 2,
		Fragment:    squashFSInvalidFragment,
		Blocks: []squashFSBlockFacts{{
			LogicalSize: squashFSDataBlockSize,
			Sparse:      true,
		}},
	}
	_, err := newSquashFSDataReader(
		context.Background(),
		bytes.NewReader(make([]byte, squashFSSuperblockSize)),
		squashFSSuperblockFacts{InodeTableStart: squashFSSuperblockSize},
		squashFSTestTree("file", squashFSExtendedRegularForm, regular),
	)
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T, want artifactContentError: %v", err, err)
	}
}

func TestSquashFSDataReaderRejectsNonExactPathsAndFragments(t *testing.T) {
	fragment := squashFSRegularFacts{
		Size:     3,
		Fragment: 0,
		Offset:   7,
	}
	reader := squashFSTestDataReader(
		t,
		make([]byte, squashFSSuperblockSize),
		"dir/file",
		squashFSBasicRegularForm,
		fragment,
	)
	for _, path := range []string{"./dir/file", "dir/../dir/file", "missing", "dir/file"} {
		_, err := reader.Open(context.Background(), path)
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("Open(%q) error = %T, want artifactInfrastructureError", path, err)
		}
	}
}

func TestSquashFSDataReaderRejectsInvalidBlockContent(t *testing.T) {
	valid := squashFSTestCompressedData(t, []byte("content"))
	badChecksum := squashFSTestChecksummedData(t, []byte("content"))
	badChecksum[len(badChecksum)-1] ^= 0xff
	tests := []struct {
		name         string
		payload      []byte
		logicalSize  uint32
		uncompressed bool
		storedSize   uint32
	}{
		{
			name:        "short decoded output",
			payload:     valid,
			logicalSize: 8,
		},
		{
			name:        "long decoded output",
			payload:     valid,
			logicalSize: 6,
		},
		{
			name:        "trailing byte",
			payload:     append(append([]byte(nil), valid...), 0),
			logicalSize: 7,
		},
		{
			name:        "concatenated frame",
			payload:     append(append([]byte(nil), valid...), valid...),
			logicalSize: 7,
		},
		{
			name:        "bad checksum",
			payload:     badChecksum,
			logicalSize: 7,
		},
		{
			name:         "uncompressed length mismatch",
			payload:      []byte("content"),
			logicalSize:  6,
			uncompressed: true,
		},
		{
			name:        "oversized stored payload",
			payload:     []byte{0},
			logicalSize: 1,
			storedSize:  squashFSDataBlockSize + 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image := make([]byte, squashFSSuperblockSize)
			start := uint64(len(image))
			image = append(image, test.payload...)
			storedSize := test.storedSize
			if storedSize == 0 {
				storedSize = uint32(len(test.payload))
			}
			regular := squashFSRegularFacts{
				Size:     uint64(test.logicalSize),
				Fragment: squashFSInvalidFragment,
				Blocks: []squashFSBlockFacts{{
					StoredSize:   storedSize,
					LogicalSize:  test.logicalSize,
					Uncompressed: test.uncompressed,
					Start:        start,
					End:          start + uint64(storedSize),
				}},
			}
			_, err := newSquashFSDataReader(
				context.Background(),
				bytes.NewReader(image),
				squashFSSuperblockFacts{InodeTableStart: uint64(len(image))},
				squashFSTestTree("file", squashFSBasicRegularForm, regular),
			)
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError: %v", err, err)
			}
		})
	}
}

func TestSquashFSDataReaderDoesNotReadOverlappingPayloads(t *testing.T) {
	var reads int
	source := squashFSReaderAtFunc(func([]byte, int64) (int, error) {
		reads++
		return 0, errors.New("unexpected read")
	})
	block := squashFSBlockFacts{
		StoredSize:   1,
		LogicalSize:  1,
		Uncompressed: true,
		Start:        squashFSSuperblockSize,
		End:          squashFSSuperblockSize + 1,
	}
	regular := squashFSRegularFacts{
		Size:     1,
		Fragment: squashFSInvalidFragment,
		Blocks:   []squashFSBlockFacts{block},
	}
	tree := squashFSTreeFacts{
		HasOverlappingData: true,
		Paths: []squashFSPathFacts{
			squashFSTestRegularPath("first", squashFSBasicRegularForm, regular),
			squashFSTestRegularPath("second", squashFSBasicRegularForm, regular),
		},
	}
	reader, err := newSquashFSDataReader(
		context.Background(),
		source,
		squashFSSuperblockFacts{InodeTableStart: squashFSSuperblockSize + 1},
		tree,
	)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 0 {
		t.Fatalf("backing reads = %d, want 0", reads)
	}
	_, err = reader.Open(context.Background(), "first")
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("Open error = %T, want artifactInfrastructureError", err)
	}
}

func TestSquashFSDataReaderPreservesInfrastructureErrors(t *testing.T) {
	payload := []byte("content")
	regular := squashFSRegularFacts{
		Size:     uint64(len(payload)),
		Fragment: squashFSInvalidFragment,
		Blocks: []squashFSBlockFacts{{
			StoredSize:   uint32(len(payload)),
			LogicalSize:  uint32(len(payload)),
			Uncompressed: true,
			Start:        squashFSSuperblockSize,
			End:          squashFSSuperblockSize + uint64(len(payload)),
		}},
	}
	t.Run("cancelled validation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := newSquashFSDataReader(
			ctx,
			bytes.NewReader(append(make([]byte, squashFSSuperblockSize), payload...)),
			squashFSSuperblockFacts{
				InodeTableStart: squashFSSuperblockSize + uint64(len(payload)),
			},
			squashFSTestTree("file", squashFSBasicRegularForm, regular),
		)
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError", err)
		}
	})
	t.Run("backing read failure", func(t *testing.T) {
		source := squashFSReaderAtFunc(func([]byte, int64) (int, error) {
			return 0, errors.New("storage failure")
		})
		_, err := newSquashFSDataReader(
			context.Background(),
			source,
			squashFSSuperblockFacts{
				InodeTableStart: squashFSSuperblockSize + uint64(len(payload)),
			},
			squashFSTestTree("file", squashFSBasicRegularForm, regular),
		)
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError: %v", err, err)
		}
	})
	t.Run("cancelled open reader", func(t *testing.T) {
		image := append(make([]byte, squashFSSuperblockSize), payload...)
		reader := squashFSTestDataReader(
			t,
			image,
			"file",
			squashFSBasicRegularForm,
			regular,
		)
		ctx, cancel := context.WithCancel(context.Background())
		opened, err := reader.Open(ctx, "file")
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		_, err = opened.Read(make([]byte, 1))
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError", err)
		}
	})
}

func TestSquashFSDataReaderCloseStopsReading(t *testing.T) {
	payload := []byte("content")
	image := make([]byte, squashFSSuperblockSize)
	block := squashFSTestDataBlock(t, &image, payload, true)
	reader := squashFSTestDataReader(t, image, "file", squashFSBasicRegularForm, squashFSRegularFacts{
		Size:     uint64(len(payload)),
		Fragment: squashFSInvalidFragment,
		Blocks:   []squashFSBlockFacts{block},
	})
	opened, err := reader.Open(context.Background(), "file")
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = opened.Read(make([]byte, 1))
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError", err)
	}
}

func TestSquashFSDataExtentsOverlap(t *testing.T) {
	tests := []struct {
		name    string
		extents []squashFSBlockFacts
		want    bool
	}{
		{name: "empty"},
		{
			name: "disjoint",
			extents: []squashFSBlockFacts{
				{Start: 10, End: 20},
				{Start: 30, End: 40},
			},
		},
		{
			name: "touching",
			extents: []squashFSBlockFacts{
				{Start: 20, End: 30},
				{Start: 10, End: 20},
			},
		},
		{
			name: "partial",
			extents: []squashFSBlockFacts{
				{Start: 15, End: 25},
				{Start: 10, End: 20},
			},
			want: true,
		},
		{
			name: "identical",
			extents: []squashFSBlockFacts{
				{Start: 10, End: 20},
				{Start: 10, End: 20},
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := squashFSDataExtentsOverlap(test.extents); got != test.want {
				t.Fatalf("overlap = %t, want %t", got, test.want)
			}
		})
	}
}

func FuzzSquashFSDataReader(f *testing.F) {
	f.Add([]byte("content"), uint8(3))
	f.Add(bytes.Repeat([]byte{0xa5}, 1024), uint8(31))
	f.Fuzz(func(t *testing.T, payload []byte, readSize uint8) {
		if len(payload) > squashFSDataBlockSize {
			t.Skip()
		}
		image := make([]byte, squashFSSuperblockSize)
		block := squashFSTestDataBlock(t, &image, payload, true)
		regular := squashFSRegularFacts{
			Size:     uint64(len(payload)),
			Fragment: squashFSInvalidFragment,
		}
		if len(payload) > 0 {
			regular.Blocks = []squashFSBlockFacts{block}
		}
		reader := squashFSTestDataReader(
			t,
			image,
			"file",
			squashFSBasicRegularForm,
			regular,
		)
		opened, err := reader.Open(context.Background(), "file")
		if err != nil {
			t.Fatal(err)
		}
		defer opened.Close()
		buffer := make([]byte, int(readSize)+1)
		got, err := io.ReadAll(struct{ io.Reader }{Reader: &chunkedReader{
			reader: opened,
			buffer: buffer,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("decoded %d bytes, want %d", len(got), len(payload))
		}
	})
}

func FuzzSquashFSDataBlockDecoder(f *testing.F) {
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		f.Fatal(err)
	}
	valid := writer.EncodeAll([]byte("content"), nil)
	writer.Close()
	f.Add(valid, uint32(7), uint16(0), false)
	f.Add([]byte("content"), uint32(7), uint16(0), true)
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd}, uint32(1), uint16(0), false)
	f.Fuzz(func(
		t *testing.T,
		payload []byte,
		logicalSize uint32,
		offset uint16,
		uncompressed bool,
	) {
		if len(payload) == 0 || len(payload) > squashFSDataBlockSize {
			t.Skip()
		}
		logicalSize = logicalSize%squashFSDataBlockSize + 1
		image := append(make([]byte, squashFSSuperblockSize), payload...)
		start := uint64(squashFSSuperblockSize) + uint64(offset)
		regular := squashFSRegularFacts{
			Size:     uint64(logicalSize),
			Fragment: squashFSInvalidFragment,
			Blocks: []squashFSBlockFacts{{
				StoredSize:   uint32(len(payload)),
				LogicalSize:  logicalSize,
				Uncompressed: uncompressed,
				Start:        start,
				End:          start + uint64(len(payload)),
			}},
		}
		_, err := newSquashFSDataReader(
			context.Background(),
			bytes.NewReader(image),
			squashFSSuperblockFacts{InodeTableStart: uint64(len(image))},
			squashFSTestTree("file", squashFSBasicRegularForm, regular),
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

type chunkedReader struct {
	reader io.Reader
	buffer []byte
}

func (reader *chunkedReader) Read(destination []byte) (int, error) {
	if len(destination) > len(reader.buffer) {
		destination = destination[:len(reader.buffer)]
	}
	return reader.reader.Read(destination)
}

func squashFSTestDataReader(
	t *testing.T,
	image []byte,
	path string,
	form uint16,
	regular squashFSRegularFacts,
) *squashFSDataReader {
	t.Helper()
	reader, err := newSquashFSDataReader(
		context.Background(),
		bytes.NewReader(image),
		squashFSSuperblockFacts{InodeTableStart: uint64(len(image))},
		squashFSTestTree(path, form, regular),
	)
	if err != nil {
		t.Fatal(err)
	}
	return reader
}

func squashFSTestTree(
	path string,
	form uint16,
	regular squashFSRegularFacts,
) squashFSTreeFacts {
	return squashFSTreeFacts{Paths: []squashFSPathFacts{
		squashFSTestRegularPath(path, form, regular),
	}}
}

func squashFSTestRegularPath(
	path string,
	form uint16,
	regular squashFSRegularFacts,
) squashFSPathFacts {
	return squashFSPathFacts{
		Path: path,
		Inode: &squashFSInodeFacts{
			Form:    form,
			Kind:    squashFSRegularKind,
			Regular: &regular,
		},
	}
}

func squashFSTestDataBlock(
	t *testing.T,
	image *[]byte,
	logical []byte,
	uncompressed bool,
) squashFSBlockFacts {
	t.Helper()
	stored := logical
	if !uncompressed {
		stored = squashFSTestCompressedData(t, logical)
	}
	start := uint64(len(*image))
	*image = append(*image, stored...)
	return squashFSBlockFacts{
		StoredSize:   uint32(len(stored)),
		LogicalSize:  uint32(len(logical)),
		Uncompressed: uncompressed,
		Start:        start,
		End:          uint64(len(*image)),
	}
}

func squashFSTestCompressedData(t *testing.T, data []byte) []byte {
	t.Helper()
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	return writer.EncodeAll(data, nil)
}

func squashFSTestChecksummedData(t *testing.T, data []byte) []byte {
	t.Helper()
	writer, err := zstd.NewWriter(
		nil,
		zstd.WithEncoderConcurrency(1),
		zstd.WithEncoderCRC(true),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	return writer.EncodeAll(data, nil)
}
