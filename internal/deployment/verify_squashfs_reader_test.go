package deployment

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"sync"
	"testing"
)

func TestSquashFSArtifactReaderInspectsAndOpensExactImage(t *testing.T) {
	image, content := squashFSTestArtifactImage(t, 0)
	source := &countingSquashFSReader{raw: image}
	reader, err := newSquashFSArtifactReader(
		context.Background(),
		source,
		int64(len(image)),
		programArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.Filesystem().IDs) != 0 {
		t.Fatal("ID facts were decoded before entry traversal")
	}

	inspected, err := inspectArtifact(
		context.Background(),
		reader,
		programArtifact,
		maxProgramLogicalBytes,
		int64(len(image)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.ordered) != 2 ||
		inspected.ordered[0].Path != "." ||
		inspected.ordered[1].Path != "file" {
		t.Fatalf("entries = %#v", inspected.ordered)
	}
	filesystem := reader.Filesystem()
	if len(filesystem.IDs) != 1 || filesystem.IDs[0] != 0 ||
		filesystem.InodeCount != 2 ||
		filesystem.RootInodeReference != 0 {
		t.Fatalf("filesystem = %#v", filesystem)
	}

	reads := source.ReadCount()
	if _, err := reader.Entries(context.Background()); err != nil {
		t.Fatal(err)
	}
	if source.ReadCount() != reads {
		t.Fatalf("second Entries performed %d new reads", source.ReadCount()-reads)
	}

	const parallel = 8
	errors := make(chan error, parallel)
	var group sync.WaitGroup
	for range parallel {
		group.Add(1)
		go func() {
			defer group.Done()
			opened, err := reader.Open(context.Background(), "file")
			if err != nil {
				errors <- err
				return
			}
			defer opened.Close()
			raw, err := io.ReadAll(opened)
			if err != nil {
				errors <- err
				return
			}
			if !bytes.Equal(raw, content) {
				errors <- &artifactInfrastructureError{
					cause: io.ErrUnexpectedEOF,
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
}

func TestSquashFSArtifactReaderReturnsDefensiveFacts(t *testing.T) {
	image, _ := squashFSTestArtifactImage(t, 0)
	reader, err := newSquashFSArtifactReader(
		context.Background(),
		bytes.NewReader(image),
		int64(len(image)),
		programArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := reader.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	filesystem := reader.Filesystem()
	filesystem.IDs[0] = 9
	entries[0].Path = "changed"

	again, err := reader.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reader.Filesystem().IDs[0] != 0 || again[0].Path != "." {
		t.Fatal("returned facts mutated retained reader state")
	}
}

func TestSquashFSArtifactReaderRejectsOversizedDescriptorBeforeRead(t *testing.T) {
	source := &countingSquashFSReader{}
	if _, err := newSquashFSArtifactReader(
		context.Background(),
		source,
		maxProgramPhysicalBytes+1,
		programArtifact,
	); err == nil {
		t.Fatal("oversized descriptor was accepted")
	}
	if source.ReadCount() != 0 {
		t.Fatalf("oversized descriptor performed %d reads", source.ReadCount())
	}
}

func TestSquashFSArtifactReaderLeavesHeaderPolicyToPureVerifier(t *testing.T) {
	tests := map[string]func([]byte){
		"magic": func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[0:4], 0)
		},
		"created at": func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[8:12], 1)
		},
		"block size": func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[12:16], 4096)
		},
		"fragment count": func(raw []byte) {
			binary.LittleEndian.PutUint32(raw[16:20], 1)
		},
		"compressor": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[20:22], 1)
		},
		"block log": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[22:24], 12)
		},
		"flags": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[24:26], squashFSV0Flags^1)
		},
		"id count": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[26:28], 2)
		},
		"major": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[28:30], 3)
		},
		"minor": func(raw []byte) {
			binary.LittleEndian.PutUint16(raw[30:32], 1)
		},
		"xattr table": func(raw []byte) {
			binary.LittleEndian.PutUint64(raw[56:64], 0)
		},
		"export table": func(raw []byte) {
			binary.LittleEndian.PutUint64(raw[88:96], 0)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			image, _ := squashFSTestArtifactImage(t, 0)
			mutate(image)
			reader, err := newSquashFSArtifactReader(
				context.Background(),
				bytes.NewReader(image),
				int64(len(image)),
				programArtifact,
			)
			if err != nil {
				t.Fatalf("reader rejected safely decoded facts: %v", err)
			}
			if _, err := inspectArtifact(
				context.Background(),
				reader,
				programArtifact,
				maxProgramLogicalBytes,
				int64(len(image)),
			); err == nil {
				t.Fatal("pure verifier accepted mutated header")
			}
			reader.mu.RLock()
			data := reader.data
			reader.mu.RUnlock()
			if data != nil {
				t.Fatal("header rejection traversed the filesystem")
			}
		})
	}
}

func TestSquashFSArtifactReaderSurfacesTailAndIDFacts(t *testing.T) {
	exact, _ := squashFSTestArtifactImage(t, 0)
	bytesUsed := binary.LittleEndian.Uint64(exact[40:48])
	tests := map[string][]byte{
		"truncated padding": append([]byte(nil), exact[:bytesUsed]...),
		"extra block":       append(append([]byte(nil), exact...), make([]byte, 4096)...),
		"nonzero padding": func() []byte {
			changed := append([]byte(nil), exact...)
			changed[len(changed)-1] = 1
			return changed
		}(),
	}
	for name, image := range tests {
		t.Run(name, func(t *testing.T) {
			reader, err := newSquashFSArtifactReader(
				context.Background(),
				bytes.NewReader(image),
				int64(len(image)),
				programArtifact,
			)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := inspectArtifact(
				context.Background(),
				reader,
				programArtifact,
				maxProgramLogicalBytes,
				int64(len(image)),
			); err == nil {
				t.Fatal("pure verifier accepted invalid tail facts")
			}
		})
	}

	image, _ := squashFSTestArtifactImage(t, 1)
	reader, err := newSquashFSArtifactReader(
		context.Background(),
		bytes.NewReader(image),
		int64(len(image)),
		programArtifact,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inspectArtifact(
		context.Background(),
		reader,
		programArtifact,
		maxProgramLogicalBytes,
		int64(len(image)),
	); err == nil {
		t.Fatal("pure verifier accepted a nonzero ID table")
	}
	if ids := reader.Filesystem().IDs; len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("IDs = %v", ids)
	}
}

func TestVerifySquashFSPhysical(t *testing.T) {
	t.Parallel()

	exact, _ := squashFSTestArtifactImage(t, 0)
	if err := VerifySquashFSPhysical(
		context.Background(),
		bytes.NewReader(exact),
		int64(len(exact)),
	); err != nil {
		t.Fatalf("VerifySquashFSPhysical() error = %v", err)
	}

	tests := map[string][]byte{
		"invalid superblock": func() []byte {
			changed := append([]byte(nil), exact...)
			binary.LittleEndian.PutUint16(changed[24:26], 0)
			return changed
		}(),
		"nonzero padding": func() []byte {
			changed := append([]byte(nil), exact...)
			changed[len(changed)-1] = 1
			return changed
		}(),
	}
	for name, image := range tests {
		image := image
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if err := VerifySquashFSPhysical(
				context.Background(),
				bytes.NewReader(image),
				int64(len(image)),
			); err == nil {
				t.Fatal("VerifySquashFSPhysical() accepted invalid image")
			}
		})
	}
}

func TestProjectSquashFSEntriesRetainsKnownForbiddenForms(t *testing.T) {
	tests := []squashFSInodeFacts{
		{
			Form:        squashFSExtendedSymlinkForm,
			Kind:        squashFSSymlinkKind,
			Mode:        0777,
			InodeNumber: 1,
			LinkCount:   1,
			XattrIndex:  squashFSInvalidXattr,
			LinkTarget:  "target",
		},
		{Form: squashFSBasicBlockDeviceForm, Kind: squashFSBlockDeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSBasicCharDeviceForm, Kind: squashFSCharDeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSBasicFIFOForm, Kind: squashFSFIFODeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSBasicSocketForm, Kind: squashFSSocketKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSExtendedBlockForm, Kind: squashFSBlockDeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSExtendedCharForm, Kind: squashFSCharDeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSExtendedFIFOForm, Kind: squashFSFIFODeviceKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
		{Form: squashFSExtendedSocketForm, Kind: squashFSSocketKind, InodeNumber: 1, XattrIndex: squashFSInvalidXattr},
	}
	for _, inode := range tests {
		entries, err := projectSquashFSEntries([]squashFSPathFacts{{
			Path:  ".",
			Inode: &inode,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Form != inode.Form {
			t.Fatalf("entries = %#v", entries)
		}
		if err := validateArtifactEntry(entries[0], programArtifact); err == nil {
			t.Fatalf("inode form %d reached pure verifier and was accepted", inode.Form)
		}
	}
}

func squashFSTestArtifactImage(t *testing.T, id uint32) ([]byte, []byte) {
	t.Helper()
	content := []byte("artifact content")
	image := make([]byte, squashFSSuperblockSize)
	dataStart := uint32(len(image))
	image = append(image, content...)

	directoryEntries := []squashFSTestDirectoryEntry{{
		name:        "file",
		form:        squashFSBasicRegularForm,
		reference:   32,
		inodeNumber: 2,
	}}
	root := squashFSTestArtifactInode(
		squashFSBasicDirectoryForm,
		0755,
		1,
	)
	rootBody := make([]byte, 16)
	binary.LittleEndian.PutUint32(rootBody[4:8], 2)
	binary.LittleEndian.PutUint16(
		rootBody[8:10],
		uint16(3+squashFSTestDirectoryRecordSize(directoryEntries)),
	)
	binary.LittleEndian.PutUint32(rootBody[12:16], 1)
	root = append(root, rootBody...)

	file := squashFSTestArtifactInode(
		squashFSBasicRegularForm,
		0644,
		2,
	)
	fileBody := make([]byte, 16)
	binary.LittleEndian.PutUint32(fileBody[0:4], dataStart)
	binary.LittleEndian.PutUint32(fileBody[4:8], squashFSInvalidFragment)
	binary.LittleEndian.PutUint32(fileBody[12:16], uint32(len(content)))
	file = append(file, fileBody...)
	var descriptor [4]byte
	binary.LittleEndian.PutUint32(
		descriptor[:],
		squashFSDataUncompressedBit|uint32(len(content)),
	)
	file = append(file, descriptor[:]...)

	inodeTableStart := uint64(len(image))
	image = append(
		image,
		squashFSTestMetadataBlock(t, append(root, file...), false)...,
	)
	directoryTableStart := uint64(len(image))
	image = append(
		image,
		squashFSTestMetadataBlock(
			t,
			squashFSTestDirectoryRecord(directoryEntries),
			false,
		)...,
	)
	fragmentTableStart := uint64(len(image))
	var encodedID [4]byte
	binary.LittleEndian.PutUint32(encodedID[:], id)
	image = append(
		image,
		squashFSTestMetadataBlock(t, encodedID[:], false)...,
	)
	idTableStart := uint64(len(image))
	var idPointer [8]byte
	binary.LittleEndian.PutUint64(idPointer[:], fragmentTableStart)
	image = append(image, idPointer[:]...)
	bytesUsed := uint64(len(image))

	superblock := image[:squashFSSuperblockSize]
	binary.LittleEndian.PutUint32(superblock[0:4], squashFSMagic)
	binary.LittleEndian.PutUint32(superblock[4:8], 2)
	binary.LittleEndian.PutUint32(superblock[12:16], squashFSDataBlockSize)
	binary.LittleEndian.PutUint16(superblock[20:22], squashFSZstandardCompressor)
	binary.LittleEndian.PutUint16(superblock[22:24], 17)
	binary.LittleEndian.PutUint16(superblock[24:26], squashFSV0Flags)
	binary.LittleEndian.PutUint16(superblock[26:28], 1)
	binary.LittleEndian.PutUint16(superblock[28:30], 4)
	binary.LittleEndian.PutUint64(superblock[40:48], bytesUsed)
	binary.LittleEndian.PutUint64(superblock[48:56], idTableStart)
	binary.LittleEndian.PutUint64(superblock[56:64], math.MaxUint64)
	binary.LittleEndian.PutUint64(superblock[64:72], inodeTableStart)
	binary.LittleEndian.PutUint64(superblock[72:80], directoryTableStart)
	binary.LittleEndian.PutUint64(superblock[80:88], fragmentTableStart)
	binary.LittleEndian.PutUint64(superblock[88:96], math.MaxUint64)

	physicalSize, ok := roundUpSquashFSSize(bytesUsed, squashFSPhysicalAlign)
	if !ok {
		t.Fatal("test image size overflow")
	}
	image = append(image, make([]byte, int(physicalSize-bytesUsed))...)
	return image, content
}

func squashFSTestArtifactInode(form, mode uint16, inode uint32) []byte {
	encoded := make([]byte, 16)
	binary.LittleEndian.PutUint16(encoded[0:2], form)
	binary.LittleEndian.PutUint16(encoded[2:4], mode)
	binary.LittleEndian.PutUint32(encoded[12:16], inode)
	return encoded
}

type countingSquashFSReader struct {
	mu    sync.Mutex
	raw   []byte
	reads int
}

func (reader *countingSquashFSReader) ReadAt(
	destination []byte,
	offset int64,
) (int, error) {
	reader.mu.Lock()
	reader.reads++
	reader.mu.Unlock()
	return bytes.NewReader(reader.raw).ReadAt(destination, offset)
}

func (reader *countingSquashFSReader) ReadCount() int {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.reads
}
