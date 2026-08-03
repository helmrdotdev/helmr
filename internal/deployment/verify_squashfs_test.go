package deployment

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadSquashFSFactsDecodesLittleEndianSuperblock(t *testing.T) {
	image := make([]byte, squashFSPhysicalAlign)
	littleEndian := binary.LittleEndian
	littleEndian.PutUint32(image[0:4], 0x04030201)
	littleEndian.PutUint32(image[4:8], 0x08070605)
	littleEndian.PutUint32(image[8:12], 0x0c0b0a09)
	littleEndian.PutUint32(image[12:16], 0x100f0e0d)
	littleEndian.PutUint32(image[16:20], 0x14131211)
	littleEndian.PutUint16(image[20:22], 0x1615)
	littleEndian.PutUint16(image[22:24], 0x1817)
	littleEndian.PutUint16(image[24:26], 0x1a19)
	littleEndian.PutUint16(image[26:28], 0x1c1b)
	littleEndian.PutUint16(image[28:30], 0x1e1d)
	littleEndian.PutUint16(image[30:32], 0x201f)
	littleEndian.PutUint64(image[32:40], 0x2827262524232221)
	littleEndian.PutUint64(image[40:48], 1000)
	littleEndian.PutUint64(image[48:56], 0x3837363534333231)
	littleEndian.PutUint64(image[56:64], 0x403f3e3d3c3b3a39)
	littleEndian.PutUint64(image[64:72], 0x4847464544434241)
	littleEndian.PutUint64(image[72:80], 0x504f4e4d4c4b4a49)
	littleEndian.PutUint64(image[80:88], 0x5857565554535251)
	littleEndian.PutUint64(image[88:96], 0x605f5e5d5c5b5a59)

	facts, err := readSquashFSFacts(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	want := squashFSSuperblockFacts{
		Magic:               0x04030201,
		InodeCount:          0x08070605,
		CreatedAtUnix:       0x0c0b0a09,
		BlockSize:           0x100f0e0d,
		FragmentCount:       0x14131211,
		Compressor:          0x1615,
		BlockLog:            0x1817,
		Flags:               0x1a19,
		IDCount:             0x1c1b,
		Major:               0x1e1d,
		Minor:               0x201f,
		RootInodeReference:  0x2827262524232221,
		BytesUsed:           1000,
		IDTableStart:        0x3837363534333231,
		XattrIDTableStart:   0x403f3e3d3c3b3a39,
		InodeTableStart:     0x4847464544434241,
		DirectoryTableStart: 0x504f4e4d4c4b4a49,
		FragmentTableStart:  0x5857565554535251,
		ExportTableStart:    0x605f5e5d5c5b5a59,
	}
	if facts.Superblock != want {
		t.Fatalf("superblock = %#v, want %#v", facts.Superblock, want)
	}
	if facts.PhysicalSize != squashFSPhysicalAlign {
		t.Fatalf("physical size = %d", facts.PhysicalSize)
	}
	if facts.Tail.ExpectedPhysicalSize != squashFSPhysicalAlign ||
		facts.Tail.ExpectedPaddingBytes != squashFSPhysicalAlign-1000 ||
		!facts.Tail.HasExactPhysicalSize ||
		!facts.Tail.HasZeroPadding {
		t.Fatalf("tail = %#v", facts.Tail)
	}
}

func TestReadSquashFSFactsRejectsStructuralBounds(t *testing.T) {
	tests := []struct {
		name         string
		physicalSize int64
		bytesUsed    uint64
	}{
		{name: "short physical size", physicalSize: 95, bytesUsed: 95},
		{name: "bytes used below superblock", physicalSize: 96, bytesUsed: 95},
		{name: "bytes used beyond physical size", physicalSize: 4096, bytesUsed: 4097},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image := squashFSTestImage(squashFSPhysicalAlign, test.bytesUsed)
			_, err := readSquashFSFacts(bytes.NewReader(image), test.physicalSize)
			if err == nil {
				t.Fatal("invalid structural bounds were accepted")
			}
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError", err)
			}
		})
	}
}

func TestReadSquashFSFactsReportsTailProfile(t *testing.T) {
	tests := []struct {
		name              string
		physicalSize      int64
		imageSize         int
		nonzeroOffset     int
		exactPhysicalSize bool
		zeroPadding       bool
	}{
		{
			name:              "exact zero padding",
			physicalSize:      4096,
			imageSize:         4096,
			nonzeroOffset:     -1,
			exactPhysicalSize: true,
			zeroPadding:       true,
		},
		{
			name:              "extra trailing byte",
			physicalSize:      4097,
			imageSize:         4097,
			nonzeroOffset:     4096,
			exactPhysicalSize: false,
			zeroPadding:       true,
		},
		{
			name:              "truncated padding",
			physicalSize:      2000,
			imageSize:         2000,
			nonzeroOffset:     -1,
			exactPhysicalSize: false,
			zeroPadding:       false,
		},
		{
			name:              "nonzero padding",
			physicalSize:      4096,
			imageSize:         4096,
			nonzeroOffset:     1000,
			exactPhysicalSize: true,
			zeroPadding:       false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image := squashFSTestImage(test.imageSize, 1000)
			if test.nonzeroOffset >= 0 {
				image[test.nonzeroOffset] = 1
			}
			facts, err := readSquashFSFacts(bytes.NewReader(image), test.physicalSize)
			if err != nil {
				t.Fatal(err)
			}
			if facts.Tail.ExpectedPhysicalSize != squashFSPhysicalAlign ||
				facts.Tail.ExpectedPaddingBytes != squashFSPhysicalAlign-1000 ||
				facts.Tail.HasExactPhysicalSize != test.exactPhysicalSize ||
				facts.Tail.HasZeroPadding != test.zeroPadding {
				t.Fatalf("tail = %#v", facts.Tail)
			}
		})
	}
}

func TestReadSquashFSFactsAcceptsAlignedBytesUsed(t *testing.T) {
	image := squashFSTestImage(squashFSPhysicalAlign, squashFSPhysicalAlign)
	facts, err := readSquashFSFacts(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	if facts.Tail.ExpectedPaddingBytes != 0 ||
		!facts.Tail.HasExactPhysicalSize ||
		!facts.Tail.HasZeroPadding {
		t.Fatalf("tail = %#v", facts.Tail)
	}
}

func TestReadSquashFSFactsRejectsShortReads(t *testing.T) {
	t.Run("superblock", func(t *testing.T) {
		image := squashFSTestImage(squashFSSuperblockSize-1, squashFSSuperblockSize)
		_, err := readSquashFSFacts(bytes.NewReader(image), squashFSSuperblockSize)
		if err == nil || !strings.Contains(err.Error(), "superblock") {
			t.Fatalf("error = %v", err)
		}
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError", err)
		}
	})

	t.Run("padding", func(t *testing.T) {
		image := squashFSTestImage(squashFSPhysicalAlign-1, 1000)
		_, err := readSquashFSFacts(bytes.NewReader(image), squashFSPhysicalAlign)
		if err == nil || !strings.Contains(err.Error(), "padding") {
			t.Fatalf("error = %v", err)
		}
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError", err)
		}
	})

	t.Run("short read without error", func(t *testing.T) {
		image := squashFSTestImage(squashFSPhysicalAlign, 1000)
		reader := squashFSReaderAtFunc(func(destination []byte, offset int64) (int, error) {
			count, err := bytes.NewReader(image).ReadAt(destination, offset)
			if offset == int64(1000) {
				return count - 1, nil
			}
			return count, err
		})
		_, err := readSquashFSFacts(reader, squashFSPhysicalAlign)
		if err == nil || !strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
			t.Fatalf("error = %v", err)
		}
		var infrastructureError *artifactInfrastructureError
		if !errors.As(err, &infrastructureError) {
			t.Fatalf("error = %T, want artifactInfrastructureError", err)
		}
	})
}

func TestReadSquashFSFactsAcceptsFullReadsWithEOF(t *testing.T) {
	image := squashFSTestImage(squashFSPhysicalAlign, 1000)
	reader := squashFSReaderAtFunc(func(destination []byte, offset int64) (int, error) {
		count, err := bytes.NewReader(image).ReadAt(destination, offset)
		if count == len(destination) {
			return count, io.EOF
		}
		return count, err
	})
	facts, err := readSquashFSFacts(reader, squashFSPhysicalAlign)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.Tail.HasExactPhysicalSize || !facts.Tail.HasZeroPadding {
		t.Fatalf("tail = %#v", facts.Tail)
	}
}

func TestReadSquashFSFactsClassifiesNilReaderAsInfrastructure(t *testing.T) {
	_, err := readSquashFSFacts(nil, squashFSPhysicalAlign)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError", err)
	}
}

func TestReadSquashFSFactsClassifiesNegativePhysicalSizeAsInfrastructure(t *testing.T) {
	_, err := readSquashFSFacts(bytes.NewReader(nil), -1)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) {
		t.Fatalf("error = %T, want artifactInfrastructureError", err)
	}
}

func TestRoundUpSquashFSSizeRejectsOverflow(t *testing.T) {
	maxUint64 := ^uint64(0)
	if value, ok := roundUpSquashFSSize(maxUint64-4095, squashFSPhysicalAlign); !ok ||
		value != maxUint64-4095 {
		t.Fatalf("aligned round up = %d, %t", value, ok)
	}
	if _, ok := roundUpSquashFSSize(maxUint64, squashFSPhysicalAlign); ok {
		t.Fatal("overflow was accepted")
	}
	if _, ok := roundUpSquashFSSize(1, 0); ok {
		t.Fatal("zero alignment was accepted")
	}
}

func squashFSTestImage(size int, bytesUsed uint64) []byte {
	image := make([]byte, size)
	if len(image) >= 48 {
		binary.LittleEndian.PutUint64(image[40:48], bytesUsed)
	}
	return image
}

type squashFSReaderAtFunc func([]byte, int64) (int, error)

func (function squashFSReaderAtFunc) ReadAt(destination []byte, offset int64) (int, error) {
	return function(destination, offset)
}
