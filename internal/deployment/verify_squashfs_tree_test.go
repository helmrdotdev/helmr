package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
)

func TestReadSquashFSTreeEnumeratesRootReachableFacts(t *testing.T) {
	root := squashFSTestDirectoryInode(1, 2, 27, 0)
	file := squashFSTestBasicRegularBody(
		squashFSTestInodeBase(squashFSBasicRegularForm, 2),
	)
	directory := squashFSTestDirectoryRecord([]squashFSTestDirectoryEntry{{
		name:        "file",
		form:        squashFSBasicRegularForm,
		reference:   uint64(len(root)),
		inodeNumber: 2,
	}})
	decoder, superblock := squashFSTestTreeDecoder(t, append(root, file...), directory, 2)

	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{11, 22},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Paths) != 2 || facts.Paths[0].Path != "." ||
		facts.Paths[1].Path != "file" ||
		facts.Paths[1].Inode.Form != squashFSBasicRegularForm {
		t.Fatalf("paths = %#v", facts.Paths)
	}
	if len(facts.Edges) != 1 ||
		facts.Edges[0].ActualForm != squashFSBasicRegularForm ||
		facts.Edges[0].ActualInodeNumber != 2 {
		t.Fatalf("edges = %#v", facts.Edges)
	}
	if len(facts.Inodes) != 2 || facts.RetainedRawBytes != 5 {
		t.Fatalf("tree facts = %#v", facts)
	}
}

func TestReadSquashFSTreeAcceptsEmptyDirectoryWithoutDirectoryTable(t *testing.T) {
	root := squashFSTestDirectoryInode(1, 1, 3, 0)
	decoder, superblock := squashFSTestTreeDecoder(t, root, nil, 1)
	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Paths) != 1 || facts.Paths[0].Path != "." {
		t.Fatalf("paths = %#v", facts.Paths)
	}
}

func TestReadSquashFSTreeSurfacesFragmentReferences(t *testing.T) {
	root := squashFSTestDirectoryInode(1, 2, 27, 0)
	file := squashFSTestInodeBase(squashFSBasicRegularForm, 2)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[4:8], 0)
	binary.LittleEndian.PutUint32(body[12:16], 1)
	file = append(file, body...)
	directory := squashFSTestDirectoryRecord([]squashFSTestDirectoryEntry{{
		name:        "file",
		form:        squashFSBasicRegularForm,
		reference:   uint64(len(root)),
		inodeNumber: 2,
	}})
	decoder, superblock := squashFSTestTreeDecoder(
		t,
		append(root, file...),
		directory,
		2,
	)

	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.HasFragmentReferences {
		t.Fatal("fragment reference was not surfaced")
	}
}

func TestReadSquashFSTreeSurfacesOverlappingData(t *testing.T) {
	root := squashFSTestDirectoryInode(1, 2, 3, 0)
	first := squashFSTestRegularInodeWithExtent(2, squashFSSuperblockSize)
	second := squashFSTestRegularInodeWithExtent(3, squashFSSuperblockSize)
	directory := squashFSTestDirectoryRecord([]squashFSTestDirectoryEntry{
		{
			name:        "first",
			form:        squashFSBasicRegularForm,
			reference:   uint64(len(root)),
			inodeNumber: 2,
		},
		{
			name:        "second",
			form:        squashFSBasicRegularForm,
			reference:   uint64(len(root) + len(first)),
			inodeNumber: 3,
		},
	})
	root = squashFSTestDirectoryInode(
		1,
		2,
		uint16(len(directory)+3),
		0,
	)
	inodes := append(append(root, first...), second...)
	decoder, superblock := squashFSTestTreeDecoder(t, inodes, directory, 3)

	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.HasOverlappingData {
		t.Fatal("overlapping data extents were not surfaced")
	}
}

func TestReadSquashFSTreeReadsDirectoryAcrossMetadataBlocks(t *testing.T) {
	const inodeStart = uint64(256)
	const fileCount = 420
	inodes := make([]byte, 32+fileCount*32)
	for index := 0; index < fileCount; index++ {
		record := squashFSTestBasicRegularBody(
			squashFSTestInodeBase(squashFSBasicRegularForm, uint32(index+2)),
		)
		copy(inodes[32+index*32:], record)
	}
	entries := make([]squashFSTestDirectoryEntry, fileCount)
	for index := range entries {
		logicalOffset := uint64(32 + index*32)
		reference := logicalOffset
		if logicalOffset >= squashFSMetadataBlockSize {
			reference = uint64(squashFSMetadataBlockSize+2)<<16 |
				(logicalOffset - squashFSMetadataBlockSize)
		}
		entries[index] = squashFSTestDirectoryEntry{
			name:        fmt.Sprintf("file-%04d-xxxxxxxxxx", index),
			form:        squashFSBasicRegularForm,
			reference:   reference,
			inodeNumber: uint32(index + 2),
		}
	}
	directory := squashFSTestDirectoryRecords(entries)
	root := squashFSTestDirectoryInode(
		1,
		1,
		uint16(len(directory)+3),
		0,
	)
	copy(inodes, root)

	image := make([]byte, inodeStart)
	image = append(
		image,
		squashFSTestMetadataBlock(
			t,
			inodes[:squashFSMetadataBlockSize],
			false,
		)...,
	)
	image = append(
		image,
		squashFSTestMetadataBlock(
			t,
			inodes[squashFSMetadataBlockSize:],
			false,
		)...,
	)
	directoryStart := uint64(len(image))
	image = append(
		image,
		squashFSTestMetadataBlock(
			t,
			directory[:squashFSMetadataBlockSize],
			false,
		)...,
	)
	image = append(
		image,
		squashFSTestMetadataBlock(
			t,
			directory[squashFSMetadataBlockSize:],
			false,
		)...,
	)
	superblock := squashFSSuperblockFacts{
		InodeCount:          fileCount + 1,
		BlockSize:           131072,
		InodeTableStart:     inodeStart,
		DirectoryTableStart: directoryStart,
		FragmentTableStart:  uint64(len(image)),
		BytesUsed:           uint64(len(image)),
	}
	decoder, err := newSquashFSMetadataDecoder(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)

	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Paths) != fileCount+1 ||
		facts.Paths[1].Path != "file-0000-xxxxxxxxxx" ||
		facts.Paths[fileCount].Path != "file-0419-xxxxxxxxxx" {
		t.Fatalf("paths = %#v", facts.Paths)
	}
}

func TestReadSquashFSTreeRetainsKnownForbiddenRoot(t *testing.T) {
	root := squashFSTestBasicDeviceBody(
		squashFSTestInodeBase(squashFSBasicBlockDeviceForm, 1),
	)
	decoder, superblock := squashFSTestTreeDecoder(t, root, nil, 1)
	facts, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Paths) != 1 ||
		facts.Paths[0].Inode.Kind != squashFSBlockDeviceKind {
		t.Fatalf("paths = %#v", facts.Paths)
	}
}

func TestReadSquashFSTreeRejectsUnusedDecodedMetadata(t *testing.T) {
	root := append(squashFSTestDirectoryInode(1, 1, 3, 0), 0)
	decoder, superblock := squashFSTestTreeDecoder(t, root, nil, 1)
	_, err := readSquashFSTree(
		context.Background(),
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	var contentError *artifactContentError
	if !errors.As(err, &contentError) {
		t.Fatalf("error = %T, want artifactContentError: %v", err, err)
	}
}

func TestValidateSquashFSMetadataCoverage(t *testing.T) {
	tests := []struct {
		name   string
		ranges []squashFSMetadataRange
		size   uint64
		ok     bool
	}{
		{
			name: "exact",
			ranges: []squashFSMetadataRange{
				{start: 3, end: 5},
				{start: 0, end: 3},
			},
			size: 5,
			ok:   true,
		},
		{
			name: "gap",
			ranges: []squashFSMetadataRange{
				{start: 0, end: 2},
				{start: 3, end: 5},
			},
			size: 5,
		},
		{
			name: "overlap",
			ranges: []squashFSMetadataRange{
				{start: 0, end: 4},
				{start: 3, end: 5},
			},
			size: 5,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSquashFSMetadataCoverage(test.ranges, test.size)
			if (err == nil) != test.ok {
				t.Fatalf("error = %v, ok = %t", err, test.ok)
			}
		})
	}
}

func TestReadSquashFSTreeRejectsGraphAndDirectoryViolations(t *testing.T) {
	tests := []struct {
		name             string
		entries          []squashFSTestDirectoryEntry
		inodeCount       uint32
		thirdInodeNumber uint32
	}{
		{
			name: "declared form mismatch",
			entries: []squashFSTestDirectoryEntry{{
				name: "file", form: squashFSBasicSymlinkForm, reference: 32, inodeNumber: 2,
			}},
			inodeCount: 2,
		},
		{
			name: "repeated reference",
			entries: []squashFSTestDirectoryEntry{
				{name: "a", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2},
				{name: "b", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2},
			},
			inodeCount: 3,
		},
		{
			name: "directory cycle",
			entries: []squashFSTestDirectoryEntry{{
				name: "self", form: squashFSBasicDirectoryForm, reference: 0, inodeNumber: 1,
			}},
			inodeCount: 2,
		},
		{
			name: "duplicate inode number",
			entries: []squashFSTestDirectoryEntry{
				{name: "a", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2},
				{name: "b", form: squashFSBasicRegularForm, reference: 64, inodeNumber: 2},
			},
			inodeCount:       3,
			thirdInodeNumber: 2,
		},
		{
			name: "names out of order",
			entries: []squashFSTestDirectoryEntry{
				{name: "b", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2},
				{name: "a", form: squashFSBasicRegularForm, reference: 64, inodeNumber: 3},
			},
			inodeCount: 3,
		},
		{
			name: "unreachable inode count",
			entries: []squashFSTestDirectoryEntry{{
				name: "file", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2,
			}},
			inodeCount: 3,
		},
		{
			name: "invalid component",
			entries: []squashFSTestDirectoryEntry{{
				name: "..", form: squashFSBasicRegularForm, reference: 32, inodeNumber: 2,
			}},
			inodeCount: 2,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := squashFSTestDirectoryInode(
				1,
				2,
				uint16(3+squashFSTestDirectoryRecordSize(test.entries)),
				0,
			)
			inodes := append([]byte{}, root...)
			inodes = append(
				inodes,
				squashFSTestBasicRegularBody(
					squashFSTestInodeBase(squashFSBasicRegularForm, 2),
				)...,
			)
			if len(test.entries) > 1 && test.entries[1].reference == 64 {
				inodeNumber := test.thirdInodeNumber
				if inodeNumber == 0 {
					inodeNumber = 3
				}
				inodes = append(
					inodes,
					squashFSTestBasicRegularBody(
						squashFSTestInodeBase(squashFSBasicRegularForm, inodeNumber),
					)...,
				)
			}
			directory := squashFSTestDirectoryRecord(test.entries)
			decoder, superblock := squashFSTestTreeDecoder(
				t,
				inodes,
				directory,
				test.inodeCount,
			)
			_, err := readSquashFSTree(
				context.Background(),
				decoder,
				superblock,
				[]uint32{0, 0},
				uint64(maxCodeLogicalBytes),
			)
			var contentError *artifactContentError
			if !errors.As(err, &contentError) {
				t.Fatalf("error = %T, want artifactContentError: %v", err, err)
			}
		})
	}
}

func TestReadSquashFSTreeHonorsCancellation(t *testing.T) {
	root := squashFSTestDirectoryInode(1, 1, 3, 0)
	decoder, superblock := squashFSTestTreeDecoder(t, root, nil, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readSquashFSTree(
		ctx,
		decoder,
		superblock,
		[]uint32{0, 0},
		uint64(maxCodeLogicalBytes),
	)
	var infrastructureError *artifactInfrastructureError
	if !errors.As(err, &infrastructureError) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %T %v, want canceled infrastructure error", err, err)
	}
}

func FuzzReadSquashFSTree(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0}, []byte{})
	f.Add(squashFSTestDirectoryInode(1, 1, 3, 0), []byte{})
	f.Fuzz(func(t *testing.T, inodeData, directoryData []byte) {
		if len(inodeData) == 0 || len(inodeData) > squashFSMetadataBlockSize ||
			len(directoryData) > squashFSMetadataBlockSize {
			return
		}
		decoder, superblock := squashFSTestTreeDecoder(
			t,
			inodeData,
			directoryData,
			1,
		)
		_, err := readSquashFSTree(
			context.Background(),
			decoder,
			superblock,
			[]uint32{0},
			uint64(maxCodeLogicalBytes),
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

type squashFSTestDirectoryEntry struct {
	name        string
	form        uint16
	reference   uint64
	inodeNumber uint32
}

func squashFSTestTreeDecoder(
	t *testing.T,
	inodeData []byte,
	directoryData []byte,
	inodeCount uint32,
) (*squashFSMetadataDecoder, squashFSSuperblockFacts) {
	t.Helper()
	const inodeStart = uint64(256)
	image := make([]byte, inodeStart)
	image = append(image, squashFSTestMetadataBlock(t, inodeData, false)...)
	directoryStart := uint64(len(image))
	if len(directoryData) > 0 {
		image = append(image, squashFSTestMetadataBlock(t, directoryData, false)...)
	}
	superblock := squashFSSuperblockFacts{
		InodeCount:          inodeCount,
		BlockSize:           131072,
		RootInodeReference:  0,
		InodeTableStart:     inodeStart,
		DirectoryTableStart: directoryStart,
		FragmentTableStart:  uint64(len(image)),
		BytesUsed:           uint64(len(image)),
	}
	decoder, err := newSquashFSMetadataDecoder(bytes.NewReader(image))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(decoder.Close)
	return decoder, superblock
}

func squashFSTestRegularInodeWithExtent(
	inodeNumber uint32,
	start uint32,
) []byte {
	encoded := squashFSTestInodeBase(squashFSBasicRegularForm, inodeNumber)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[0:4], start)
	binary.LittleEndian.PutUint32(body[4:8], squashFSInvalidFragment)
	binary.LittleEndian.PutUint32(body[12:16], 1)
	encoded = append(encoded, body...)
	var descriptor [4]byte
	binary.LittleEndian.PutUint32(
		descriptor[:],
		squashFSDataUncompressedBit|1,
	)
	return append(encoded, descriptor[:]...)
}

func squashFSTestDirectoryInode(
	inodeNumber uint32,
	parent uint32,
	encodedSize uint16,
	offset uint16,
) []byte {
	encoded := squashFSTestInodeBase(squashFSBasicDirectoryForm, inodeNumber)
	body := make([]byte, 16)
	binary.LittleEndian.PutUint32(body[4:8], 2)
	binary.LittleEndian.PutUint16(body[8:10], encodedSize)
	binary.LittleEndian.PutUint16(body[10:12], offset)
	binary.LittleEndian.PutUint32(body[12:16], parent)
	return append(encoded, body...)
}

func squashFSTestDirectoryRecord(
	entries []squashFSTestDirectoryEntry,
) []byte {
	return squashFSTestDirectoryRecords(entries)
}

func squashFSTestDirectoryRecords(
	entries []squashFSTestDirectoryEntry,
) []byte {
	if len(entries) == 0 {
		return nil
	}
	var encoded []byte
	for len(entries) > 0 {
		count := 1
		block := entries[0].reference >> 16
		for count < len(entries) && count < 256 &&
			entries[count].reference>>16 == block {
			count++
		}
		header := make([]byte, 12)
		binary.LittleEndian.PutUint32(header[0:4], uint32(count-1))
		binary.LittleEndian.PutUint32(header[4:8], uint32(block))
		binary.LittleEndian.PutUint32(header[8:12], entries[0].inodeNumber)
		encoded = append(encoded, header...)
		for _, entry := range entries[:count] {
			item := make([]byte, 8)
			binary.LittleEndian.PutUint16(item[0:2], uint16(entry.reference))
			delta := int64(entry.inodeNumber) - int64(entries[0].inodeNumber)
			binary.LittleEndian.PutUint16(item[2:4], uint16(int16(delta)))
			binary.LittleEndian.PutUint16(item[4:6], entry.form)
			binary.LittleEndian.PutUint16(item[6:8], uint16(len(entry.name)-1))
			encoded = append(encoded, item...)
			encoded = append(encoded, entry.name...)
		}
		entries = entries[count:]
	}
	return encoded
}

func squashFSTestDirectoryRecordSize(
	entries []squashFSTestDirectoryEntry,
) int {
	return len(squashFSTestDirectoryRecords(entries))
}
