package deployment

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func TestSquashFSInodeDecoderReadsKnownForms(t *testing.T) {
	tests := []struct {
		form uint16
		kind squashFSInodeKind
		body func([]byte) []byte
	}{
		{squashFSBasicDirectoryForm, squashFSDirectoryKind, squashFSTestBasicDirectoryBody},
		{squashFSBasicRegularForm, squashFSRegularKind, squashFSTestBasicRegularBody},
		{squashFSBasicSymlinkForm, squashFSSymlinkKind, squashFSTestBasicSymlinkBody},
		{squashFSBasicBlockDeviceForm, squashFSBlockDeviceKind, squashFSTestBasicDeviceBody},
		{squashFSBasicCharDeviceForm, squashFSCharDeviceKind, squashFSTestBasicDeviceBody},
		{squashFSBasicFIFOForm, squashFSFIFODeviceKind, squashFSTestBasicIPCBody},
		{squashFSBasicSocketForm, squashFSSocketKind, squashFSTestBasicIPCBody},
		{squashFSExtendedDirectoryForm, squashFSDirectoryKind, squashFSTestExtendedDirectoryBody},
		{squashFSExtendedRegularForm, squashFSRegularKind, squashFSTestExtendedRegularBody},
		{squashFSExtendedSymlinkForm, squashFSSymlinkKind, squashFSTestExtendedSymlinkBody},
		{squashFSExtendedBlockForm, squashFSBlockDeviceKind, squashFSTestExtendedDeviceBody},
		{squashFSExtendedCharForm, squashFSCharDeviceKind, squashFSTestExtendedDeviceBody},
		{squashFSExtendedFIFOForm, squashFSFIFODeviceKind, squashFSTestExtendedIPCBody},
		{squashFSExtendedSocketForm, squashFSSocketKind, squashFSTestExtendedIPCBody},
	}
	for _, test := range tests {
		t.Run(squashFSTestFormName(test.form), func(t *testing.T) {
			record := squashFSTestInodeBase(test.form, 17)
			record = test.body(record)
			decoder := squashFSTestInodeDecoder(t, record, 256)
			inode, err := decoder.read(0)
			if err != nil {
				t.Fatal(err)
			}
			if inode.Form != test.form || inode.Kind != test.kind {
				t.Fatalf("inode form/kind = %d/%d, want %d/%d", inode.Form, inode.Kind, test.form, test.kind)
			}
			if inode.InodeNumber != 17 || inode.UID != 11 || inode.GID != 22 {
				t.Fatalf("inode identity = %#v", inode)
			}
			if test.form == squashFSBasicRegularForm && inode.LinkCount != 1 {
				t.Fatalf("basic regular link count = %d, want 1", inode.LinkCount)
			}
		})
	}
}

func TestSquashFSInodeDecoderReadsAcrossMetadataBlocks(t *testing.T) {
	record := squashFSTestBasicSymlinkBody(
		squashFSTestInodeBase(squashFSBasicSymlinkForm, 1),
	)
	firstData := make([]byte, squashFSMetadataBlockSize)
	start := len(firstData) - 8
	copy(firstData[start:], record[:8])
	first := squashFSTestMetadataBlock(t, firstData, false)
	second := squashFSTestMetadataBlock(t, record[8:], false)
	inodeStart := uint64(256)
	image := make([]byte, inodeStart)
	image = append(image, first...)
	image = append(image, second...)
	superblock := squashFSTestInodeSuperblock(inodeStart, uint64(len(image)))
	decoder := newSquashFSTestMetadataDecoder(t, image)
	inodes, err := newSquashFSInodeDecoder(
		context.Background(),
		decoder,
		superblock,
		[]uint32{11, 22},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	inode, err := inodes.read(uint64(start))
	if err != nil {
		t.Fatal(err)
	}
	if inode.Form != squashFSBasicSymlinkForm || inode.LinkTarget != "target" {
		t.Fatalf("inode = %#v", inode)
	}
}

func TestSquashFSMetadataCacheRejectsInteriorBlockOffset(t *testing.T) {
	payload := make([]byte, 200)
	payload[0] = 0x80
	image := squashFSTestMetadataBlock(t, payload, false)
	decoder := newSquashFSTestMetadataDecoder(t, image)
	cache, err := newSquashFSMetadataCache(
		context.Background(),
		decoder,
		squashFSRegion{end: uint64(len(image))},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cache.block(1)
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T, want artifactContentError: %v", err, err)
	}
}

func TestSquashFSMetadataCacheRequiresCompleteRegion(t *testing.T) {
	image := append(
		squashFSTestMetadataBlock(t, []byte("inode"), false),
		0,
	)
	decoder := newSquashFSTestMetadataDecoder(t, image)
	cache, err := newSquashFSMetadataCache(
		context.Background(),
		decoder,
		squashFSRegion{end: uint64(len(image))},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.block(0); err != nil {
		t.Fatal(err)
	}
	err = cache.validateComplete()
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T, want artifactContentError: %v", err, err)
	}
}

func TestSquashFSInodeDecoderRetainsRegularBlocks(t *testing.T) {
	base := squashFSTestInodeBase(squashFSBasicRegularForm, 1)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[0:4], 96)
	binary.LittleEndian.PutUint32(body[4:8], squashFSInvalidFragment)
	binary.LittleEndian.PutUint32(body[12:16], 131073)
	record := append(base, body...)
	var descriptors [8]byte
	binary.LittleEndian.PutUint32(descriptors[4:8], squashFSDataUncompressedBit|3)
	record = append(record, descriptors[:]...)

	decoder := squashFSTestInodeDecoder(t, record, 256)
	inode, err := decoder.read(0)
	if err != nil {
		t.Fatal(err)
	}
	blocks := inode.Regular.Blocks
	if len(blocks) != 2 || !blocks[0].Sparse ||
		blocks[0].LogicalSize != 131072 ||
		blocks[1].Start != 96 || blocks[1].End != 99 ||
		blocks[1].StoredSize != 3 || !blocks[1].Uncompressed ||
		blocks[1].LogicalSize != 1 {
		t.Fatalf("blocks = %#v", blocks)
	}
}

func TestSquashFSInodeDecoderRetainsExtendedDirectoryIndexes(t *testing.T) {
	record := squashFSTestInodeBase(squashFSExtendedDirectoryForm, 1)
	body := make([]byte, 24)
	binary.LittleEndian.PutUint32(body[0:4], 2)
	binary.LittleEndian.PutUint32(body[4:8], 3)
	binary.LittleEndian.PutUint32(body[12:16], 1)
	binary.LittleEndian.PutUint16(body[16:18], 1)
	binary.LittleEndian.PutUint32(body[20:24], squashFSInvalidXattr)
	record = append(record, body...)
	index := make([]byte, 12)
	binary.LittleEndian.PutUint32(index[0:4], 7)
	binary.LittleEndian.PutUint32(index[4:8], 9)
	binary.LittleEndian.PutUint32(index[8:12], 2)
	record = append(record, index...)
	record = append(record, "key"...)

	decoder := squashFSTestInodeDecoder(t, record, 256)
	inode, err := decoder.read(0)
	if err != nil {
		t.Fatal(err)
	}
	indexes := inode.Directory.Indexes
	if len(indexes) != 1 || indexes[0].Index != 7 ||
		indexes[0].StartBlock != 9 || indexes[0].Name != "key" {
		t.Fatalf("indexes = %#v", indexes)
	}
}

func TestSquashFSInodeDecoderPreservesExtendedRegularFields(t *testing.T) {
	record := squashFSTestInodeBase(squashFSExtendedRegularForm, 1)
	body := make([]byte, 40)
	binary.LittleEndian.PutUint64(body[0:8], 123)
	binary.LittleEndian.PutUint64(body[8:16], 1)
	binary.LittleEndian.PutUint64(body[16:24], 7)
	binary.LittleEndian.PutUint32(body[24:28], 3)
	binary.LittleEndian.PutUint32(body[28:32], squashFSInvalidFragment)
	binary.LittleEndian.PutUint32(body[32:36], 9)
	binary.LittleEndian.PutUint32(body[36:40], 10)
	record = append(record, body...)
	record = append(record, 0, 0, 0, 0)

	decoder := squashFSTestInodeDecoder(t, record, 256)
	inode, err := decoder.read(0)
	if err != nil {
		t.Fatal(err)
	}
	if inode.LinkCount != 3 || inode.XattrIndex != 10 ||
		inode.Regular.StartBlock != 123 ||
		inode.Regular.Size != 1 ||
		inode.Regular.SparseBytes != 7 ||
		inode.Regular.Fragment != squashFSInvalidFragment ||
		inode.Regular.Offset != 9 {
		t.Fatalf("inode = %#v", inode)
	}
}

func TestSquashFSInodeDecoderPreservesRemainingFormFields(t *testing.T) {
	tests := []struct {
		name   string
		record []byte
		check  func(squashFSInodeFacts) bool
	}{
		{
			name: "basic directory",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSBasicDirectoryForm, 1)
				body := make([]byte, 16)
				binary.LittleEndian.PutUint32(body[0:4], 5)
				binary.LittleEndian.PutUint32(body[4:8], 6)
				binary.LittleEndian.PutUint16(body[8:10], 7)
				binary.LittleEndian.PutUint16(body[10:12], 8)
				binary.LittleEndian.PutUint32(body[12:16], 9)
				return append(record, body...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 6 &&
					inode.Directory.StartBlock == 5 &&
					inode.Directory.EncodedSize == 7 &&
					inode.Directory.Offset == 8 &&
					inode.Directory.ParentInode == 9
			},
		},
		{
			name: "extended directory",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSExtendedDirectoryForm, 1)
				body := make([]byte, 24)
				binary.LittleEndian.PutUint32(body[0:4], 5)
				binary.LittleEndian.PutUint32(body[4:8], 6)
				binary.LittleEndian.PutUint32(body[8:12], 7)
				binary.LittleEndian.PutUint32(body[12:16], 8)
				binary.LittleEndian.PutUint16(body[18:20], 9)
				binary.LittleEndian.PutUint32(body[20:24], 10)
				return append(record, body...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 5 && inode.XattrIndex == 10 &&
					inode.Directory.EncodedSize == 6 &&
					inode.Directory.StartBlock == 7 &&
					inode.Directory.ParentInode == 8 &&
					inode.Directory.Offset == 9
			},
		},
		{
			name: "extended symlink",
			record: func() []byte {
				record := squashFSTestBasicSymlinkBody(
					squashFSTestInodeBase(squashFSExtendedSymlinkForm, 1),
				)
				var xattr [4]byte
				binary.LittleEndian.PutUint32(xattr[:], 9)
				return append(record, xattr[:]...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 1 &&
					inode.LinkTarget == "target" &&
					inode.XattrIndex == 9
			},
		},
		{
			name: "basic device",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSBasicBlockDeviceForm, 1)
				body := make([]byte, 8)
				binary.LittleEndian.PutUint32(body[0:4], 5)
				binary.LittleEndian.PutUint32(body[4:8], 6)
				return append(record, body...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 5 && inode.Device == 6
			},
		},
		{
			name: "extended device",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSExtendedCharForm, 1)
				body := make([]byte, 12)
				binary.LittleEndian.PutUint32(body[0:4], 5)
				binary.LittleEndian.PutUint32(body[4:8], 6)
				binary.LittleEndian.PutUint32(body[8:12], 7)
				return append(record, body...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 5 &&
					inode.Device == 6 &&
					inode.XattrIndex == 7
			},
		},
		{
			name: "basic IPC",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSBasicSocketForm, 1)
				var body [4]byte
				binary.LittleEndian.PutUint32(body[:], 5)
				return append(record, body[:]...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 5
			},
		},
		{
			name: "extended IPC",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSExtendedFIFOForm, 1)
				var body [8]byte
				binary.LittleEndian.PutUint32(body[0:4], 5)
				binary.LittleEndian.PutUint32(body[4:8], 6)
				return append(record, body[:]...)
			}(),
			check: func(inode squashFSInodeFacts) bool {
				return inode.LinkCount == 5 && inode.XattrIndex == 6
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decoder := squashFSTestInodeDecoder(t, test.record, 256)
			inode, err := decoder.read(0)
			if err != nil {
				t.Fatal(err)
			}
			if !test.check(inode) {
				t.Fatalf("inode = %#v", inode)
			}
		})
	}
}

func TestSquashFSInodeDecoderUsesFragmentBlockCount(t *testing.T) {
	base := squashFSTestInodeBase(squashFSBasicRegularForm, 1)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[0:4], 96)
	binary.LittleEndian.PutUint32(body[4:8], 0)
	binary.LittleEndian.PutUint32(body[8:12], 7)
	binary.LittleEndian.PutUint32(body[12:16], 131073)
	record := append(base, body...)
	var descriptor [4]byte
	binary.LittleEndian.PutUint32(descriptor[:], squashFSDataUncompressedBit|4)
	record = append(record, descriptor[:]...)

	decoder := squashFSTestInodeDecoder(t, record, 256)
	inode, err := decoder.read(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(inode.Regular.Blocks) != 1 || inode.Regular.Offset != 7 {
		t.Fatalf("regular facts = %#v", inode.Regular)
	}
}

func TestSquashFSInodeDecoderRejectsInvalidFacts(t *testing.T) {
	tests := []struct {
		name      string
		record    []byte
		reference uint64
		ids       []uint32
	}{
		{
			name:   "unknown form",
			record: squashFSTestInodeBase(15, 1),
			ids:    []uint32{0, 0},
		},
		{
			name:   "UID index",
			record: squashFSTestInodeBase(squashFSBasicFIFOForm, 1),
			ids:    []uint32{0},
		},
		{
			name:      "noncanonical reference",
			record:    squashFSTestBasicIPCBody(squashFSTestInodeBase(squashFSBasicFIFOForm, 1)),
			reference: uint64(1) << 48,
			ids:       []uint32{0, 0},
		},
		{
			name:   "truncated variable record",
			record: squashFSTestInodeBase(squashFSBasicSymlinkForm, 1),
			ids:    []uint32{0, 0},
		},
		{
			name: "negative extended start block",
			record: func() []byte {
				record := squashFSTestInodeBase(squashFSExtendedRegularForm, 1)
				body := make([]byte, 40)
				binary.LittleEndian.PutUint64(body[0:8], ^uint64(0))
				binary.LittleEndian.PutUint32(body[28:32], squashFSInvalidFragment)
				return append(record, body...)
			}(),
			ids: []uint32{0, 0},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inodeStart := uint64(256)
			image := make([]byte, inodeStart)
			image = append(image, squashFSTestMetadataBlock(t, test.record, false)...)
			superblock := squashFSTestInodeSuperblock(inodeStart, uint64(len(image)))
			metadata := newSquashFSTestMetadataDecoder(t, image)
			decoder, err := newSquashFSInodeDecoder(
				context.Background(),
				metadata,
				superblock,
				test.ids,
				uint64(maxCodeLogicalBytes),
			)
			if err != nil {
				t.Fatal(err)
			}
			_, err = decoder.read(test.reference)
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError: %v", err, err)
			}
		})
	}
}

func TestCeilSquashFSDivide(t *testing.T) {
	tests := []struct {
		value   uint64
		divisor uint64
		want    uint64
		ok      bool
	}{
		{value: 0, divisor: 131072, want: 0, ok: true},
		{value: 1, divisor: 131072, want: 1, ok: true},
		{value: 131072, divisor: 131072, want: 1, ok: true},
		{value: 131073, divisor: 131072, want: 2, ok: true},
		{value: 1, divisor: 0, ok: false},
	}
	for _, test := range tests {
		got, ok := ceilSquashFSDivide(test.value, test.divisor)
		if got != test.want || ok != test.ok {
			t.Fatalf(
				"ceilSquashFSDivide(%d, %d) = %d, %t, want %d, %t",
				test.value,
				test.divisor,
				got,
				ok,
				test.want,
				test.ok,
			)
		}
	}
}

func squashFSTestInodeDecoder(
	t *testing.T,
	record []byte,
	inodeStart uint64,
) *squashFSInodeDecoder {
	t.Helper()
	image := make([]byte, inodeStart)
	image = append(image, squashFSTestMetadataBlock(t, record, false)...)
	superblock := squashFSTestInodeSuperblock(inodeStart, uint64(len(image)))
	metadata := newSquashFSTestMetadataDecoder(t, image)
	decoder, err := newSquashFSInodeDecoder(
		context.Background(),
		metadata,
		superblock,
		[]uint32{11, 22},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	return decoder
}

func squashFSTestInodeSuperblock(
	inodeStart uint64,
	tableEnd uint64,
) squashFSSuperblockFacts {
	return squashFSSuperblockFacts{
		BlockSize:           131072,
		InodeTableStart:     inodeStart,
		DirectoryTableStart: tableEnd,
		FragmentTableStart:  tableEnd,
		BytesUsed:           tableEnd,
	}
}

func squashFSTestInodeBase(form uint16, inode uint32) []byte {
	encoded := make([]byte, 16)
	binary.LittleEndian.PutUint16(encoded[0:2], form)
	binary.LittleEndian.PutUint16(encoded[2:4], 0755)
	binary.LittleEndian.PutUint16(encoded[4:6], 0)
	binary.LittleEndian.PutUint16(encoded[6:8], 1)
	binary.LittleEndian.PutUint32(encoded[8:12], 23)
	binary.LittleEndian.PutUint32(encoded[12:16], inode)
	return encoded
}

func squashFSTestBasicDirectoryBody(encoded []byte) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[4:8], 2)
	binary.LittleEndian.PutUint16(body[8:10], 3)
	binary.LittleEndian.PutUint32(body[12:16], 17)
	return append(encoded, body...)
}

func squashFSTestExtendedDirectoryBody(encoded []byte) []byte {
	body := make([]byte, 24)
	binary.LittleEndian.PutUint32(body[0:4], 2)
	binary.LittleEndian.PutUint32(body[4:8], 3)
	binary.LittleEndian.PutUint32(body[12:16], 17)
	binary.LittleEndian.PutUint32(body[20:24], squashFSInvalidXattr)
	return append(encoded, body...)
}

func squashFSTestBasicRegularBody(encoded []byte) []byte {
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[4:8], squashFSInvalidFragment)
	return append(encoded, body...)
}

func squashFSTestExtendedRegularBody(encoded []byte) []byte {
	body := make([]byte, 40)
	binary.LittleEndian.PutUint32(body[24:28], 1)
	binary.LittleEndian.PutUint32(body[28:32], squashFSInvalidFragment)
	binary.LittleEndian.PutUint32(body[36:40], squashFSInvalidXattr)
	return append(encoded, body...)
}

func squashFSTestBasicSymlinkBody(encoded []byte) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint32(body[4:8], 6)
	encoded = append(encoded, body...)
	return append(encoded, "target"...)
}

func squashFSTestExtendedSymlinkBody(encoded []byte) []byte {
	encoded = squashFSTestBasicSymlinkBody(encoded)
	var xattr [4]byte
	binary.LittleEndian.PutUint32(xattr[:], squashFSInvalidXattr)
	return append(encoded, xattr[:]...)
}

func squashFSTestBasicDeviceBody(encoded []byte) []byte {
	body := make([]byte, 8)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint32(body[4:8], 3)
	return append(encoded, body...)
}

func squashFSTestExtendedDeviceBody(encoded []byte) []byte {
	body := make([]byte, 12)
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint32(body[4:8], 3)
	binary.LittleEndian.PutUint32(body[8:12], squashFSInvalidXattr)
	return append(encoded, body...)
}

func squashFSTestBasicIPCBody(encoded []byte) []byte {
	var body [4]byte
	binary.LittleEndian.PutUint32(body[:], 1)
	return append(encoded, body[:]...)
}

func squashFSTestExtendedIPCBody(encoded []byte) []byte {
	var body [8]byte
	binary.LittleEndian.PutUint32(body[0:4], 1)
	binary.LittleEndian.PutUint32(body[4:8], squashFSInvalidXattr)
	return append(encoded, body[:]...)
}

func squashFSTestFormName(form uint16) string {
	return "form-" + string(rune('A'+form-1))
}
