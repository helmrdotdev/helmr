package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"testing"
)

func TestProgramArtifactAcceptsProgram(t *testing.T) {
	program := newTestProgram(t)
	verified, err := verifyProgramArtifact(context.Background(), program.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Index().Declarations[0].DeclaredID != "build" {
		t.Fatalf("verified index = %#v", verified.Index())
	}
}

func TestProgramArtifactRejectsContractDivergence(t *testing.T) {
	tests := map[string]func(*testProgram){
		"declaration locator": func(program *testProgram) {
			program.artifact.files["helmr/declarations.json"] = []byte(
				`{"declarations":[],"formatVersion":0}`,
			)
		},
		"program entry": func(program *testProgram) {
			program.artifact.files["helmr/entry.mjs"] = []byte("process.exit(0)\n")
		},
		"unknown Platform-owned path": func(program *testProgram) {
			program.artifact.addFile("helmr/modules.json", []byte("{}"), 0o644)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			mutate(program)
			if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err == nil {
				t.Fatal("verifyProgramArtifact returned nil error")
			}
		})
	}
}

func TestProgramArtifactAcceptsLifecycleModifiedLockfile(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.files["bun.lock"] = []byte("changed by lifecycle")
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactAcceptsManagerNativeDependencyTree(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.addDirectory("node_modules/tool")
	program.artifact.addFile("node_modules/tool/package.json", []byte(`{"name":"tool"}`), 0644)
	program.artifact.addDirectory("packages")
	program.artifact.addDirectory("packages/local")
	program.artifact.addDirectory("packages/local/node_modules")
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactValidatesNamespaceLinks(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.addDirectory("node_modules/.bin")
	program.artifact.addDirectory("node_modules/tool")
	program.artifact.addFile("node_modules/tool/index.js", []byte("export {}\n"), 0644)
	program.artifact.addLink("node_modules/.bin/tool", "../tool/index.js")
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}

	escaping := newTestProgram(t)
	escaping.artifact.addLink("node_modules/escape", "../../outside")
	if _, err := verifyProgramArtifact(context.Background(), escaping.descriptor); err == nil {
		t.Fatal("verifyProgramArtifact accepted an escaping dependency link")
	}

	dangling := newTestProgram(t)
	dangling.artifact.addFile("file", []byte("x"), 0644)
	dangling.artifact.addLink("safe", "file/../..")
	if _, err := verifyProgramArtifact(context.Background(), dangling.descriptor); err != nil {
		t.Fatalf("verifyProgramArtifact rejected a confined ENOTDIR link: %v", err)
	}
}

func TestProgramArtifactAcceptsUnrelatedTypeScriptWithoutSidecars(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.addFile("source.ts", []byte("export const value = 1\n"), 0644)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactAcceptsTypeScriptDeclarationLocator(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.addFile("source.ts", []byte("export const build = {}\n"), 0644)
	locatorRaw, err := CanonicalDeclarationLocator(DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations: []LocatedDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			ModulePath: "source.ts",
			ExportName: "build",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	program.artifact.setFile("helmr/declarations.json", locatorRaw)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}

type testProgram struct {
	descriptor artifactInput
	artifact   *memoryArtifact
}

func newTestProgram(t *testing.T) *testProgram {
	t.Helper()
	lockfile := []byte("lockfileVersion = 1\n")
	programRaw, err := CanonicalProgramIndex(ProgramIndex{
		Architecture:         ArchitectureX8664,
		BuildContractVersion: ProgramBuildContractVersion,
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
		FormatVersion: ProgramIndexFormatVersion,
		Manager: ProgramManager{
			Digest:  testDigest("Manager"),
			Name:    PackageManagerBun,
			Version: "1.3.10",
		},
		RuntimeAPIVersion:       RuntimeAPIVersion,
		RuntimeDigest:           testDigest("runtime"),
		StandardToolchainDigest: testDigest("toolchain"),
		Submitted: ProgramSubmittedSource{
			LockfileDigest: digestBytes(lockfile),
			LockfileName:   "bun.lock",
			SourceDigest:   testDigest("submitted source"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	locatorRaw, err := CanonicalDeclarationLocator(DeclarationLocator{
		FormatVersion: DeclarationLocatorFormatVersion,
		Declarations: []LocatedDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			ModulePath: "build.js",
			ExportName: "build",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	artifact := newMemoryArtifact()
	artifact.addDirectory("helmr")
	artifact.addDirectory("node_modules")
	artifact.addFile("helmr/program.json", programRaw, 0644)
	artifact.addFile("helmr/declarations.json", locatorRaw, 0644)
	artifact.addFile("helmr/entry.mjs", []byte(ProgramEntry), 0644)
	artifact.addFile("build.js", []byte("export const build = {}\n"), 0644)
	artifact.addFile("package.json", []byte(`{"packageManager":"bun@1.3.10"}`), 0644)
	artifact.addFile("bun.lock", lockfile, 0644)

	return &testProgram{
		descriptor: artifactInput{
			Digest:    testDigest("Program Artifact"),
			SizeBytes: squashFSPhysicalAlign,
			MediaType: ProgramArtifactMediaType,
			Reader:    artifact,
		},
		artifact: artifact,
	}
}

func exactTestFilesystem() artifactFilesystem {
	return artifactFilesystem{
		Magic:              squashFSMagic,
		InodeCount:         1,
		BlockSize:          squashFSDataBlockSize,
		Compressor:         squashFSZstandardCompressor,
		BlockLog:           17,
		Flags:              squashFSV0Flags,
		IDCount:            1,
		Major:              4,
		Minor:              0,
		RootInodeReference: 1,
		BytesUsed:          squashFSSuperblockSize,
		PhysicalSize:       squashFSPhysicalAlign,
		XattrIDTableStart:  math.MaxUint64,
		ExportTableStart:   math.MaxUint64,
		IDs:                []uint32{0},
		HasZeroPadding:     true,
	}
}

type memoryArtifact struct {
	files      map[string][]byte
	entries    []artifactEntry
	nextInode  uint64
	filesystem artifactFilesystem
}

func newMemoryArtifact() *memoryArtifact {
	artifact := &memoryArtifact{
		files:      make(map[string][]byte),
		nextInode:  2,
		filesystem: exactTestFilesystem(),
	}
	artifact.entries = append(artifact.entries, artifactEntry{
		Path:        ".",
		Kind:        artifactEntryDirectory,
		Form:        squashFSBasicDirectoryForm,
		Mode:        0755,
		XattrIndex:  squashFSInvalidXattr,
		Inode:       1,
		InodeNumber: 1,
	})
	return artifact
}

func (artifact *memoryArtifact) Filesystem() artifactFilesystem {
	return cloneArtifactFilesystem(artifact.filesystem)
}

func (artifact *memoryArtifact) Entries(context.Context) ([]artifactEntry, error) {
	return append([]artifactEntry(nil), artifact.entries...), nil
}

func (artifact *memoryArtifact) Open(_ context.Context, path string) (io.ReadCloser, error) {
	raw, exists := artifact.files[path]
	if !exists {
		return nil, fmt.Errorf("file %q is absent", path)
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (artifact *memoryArtifact) addDirectory(path string) {
	inode := artifact.takeInode()
	artifact.entries = append(artifact.entries, artifactEntry{
		Path:        path,
		Kind:        artifactEntryDirectory,
		Form:        squashFSBasicDirectoryForm,
		Mode:        0755,
		XattrIndex:  squashFSInvalidXattr,
		Inode:       inode,
		InodeNumber: uint32(inode),
	})
}

func (artifact *memoryArtifact) addFile(path string, raw []byte, mode uint32) {
	artifact.files[path] = append([]byte(nil), raw...)
	inode := artifact.takeInode()
	artifact.entries = append(artifact.entries, artifactEntry{
		Path:        path,
		Kind:        artifactEntryRegular,
		Form:        squashFSBasicRegularForm,
		Mode:        mode,
		SizeBytes:   int64(len(raw)),
		XattrIndex:  squashFSInvalidXattr,
		Inode:       inode,
		InodeNumber: uint32(inode),
		LinkCount:   1,
	})
}

func (artifact *memoryArtifact) addLink(path, target string) {
	inode := artifact.takeInode()
	artifact.entries = append(artifact.entries, artifactEntry{
		Path:        path,
		Kind:        artifactEntrySymlink,
		Form:        squashFSBasicSymlinkForm,
		Mode:        0777,
		SizeBytes:   int64(len(target)),
		XattrIndex:  squashFSInvalidXattr,
		LinkTarget:  target,
		Inode:       inode,
		InodeNumber: uint32(inode),
		LinkCount:   1,
	})
}

func (artifact *memoryArtifact) setFile(path string, raw []byte) {
	artifact.files[path] = append([]byte(nil), raw...)
	artifact.mutate(path, func(entry *artifactEntry) {
		entry.SizeBytes = int64(len(raw))
	})
}

func (artifact *memoryArtifact) mutate(path string, mutate func(*artifactEntry)) {
	for position := range artifact.entries {
		if artifact.entries[position].Path == path {
			mutate(&artifact.entries[position])
			return
		}
	}
	panic("entry is absent: " + path)
}

func (artifact *memoryArtifact) takeInode() uint64 {
	inode := artifact.nextInode
	artifact.nextInode++
	artifact.filesystem.InodeCount++
	return inode
}

func testDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
