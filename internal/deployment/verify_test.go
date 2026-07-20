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

func TestProgramArtifactsAcceptAtomicProgram(t *testing.T) {
	pair := newProgramPair(t)
	verified, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Index().Declarations[0].DeclaredID != "build" {
		t.Fatalf("verified index = %#v", verified.Index())
	}
}

func TestProgramArtifactsRejectsContractDivergence(t *testing.T) {
	tests := map[string]func(*testProgramPair){
		"dependency descriptor": func(pair *testProgramPair) {
			pair.artifacts.Dependencies.Digest = testDigest("other dependencies")
		},
		"code dependency": func(pair *testProgramPair) {
			pair.code.addFile("node_modules/unexpected", []byte("x"), 0644)
		},
		"declaration locator": func(pair *testProgramPair) {
			pair.code.files["helmr/declarations.json"] = []byte(
				`{"declarations":[],"formatVersion":0}`,
			)
		},
		"program entry": func(pair *testProgramPair) {
			pair.code.files["helmr/entry.mjs"] = []byte("process.exit(0)\n")
		},
		"unknown Platform-owned path": func(pair *testProgramPair) {
			pair.code.addFile("helmr/modules.json", []byte("{}"), 0o644)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			pair := newProgramPair(t)
			mutate(pair)
			if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err == nil {
				t.Fatal("verifyProgramArtifacts returned nil error")
			}
		})
	}
}

func TestProgramArtifactsAcceptsLifecycleModifiedLockfile(t *testing.T) {
	pair := newProgramPair(t)
	pair.code.files["bun.lock"] = []byte("changed by lifecycle")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactsAcceptsManagerNativeDependencyTree(t *testing.T) {
	pair := newProgramPair(t)
	pair.dependencies.addDirectory("tool")
	pair.dependencies.addFile("tool/package.json", []byte(`{"name":"tool"}`), 0644)
	pair.code.addDirectory("packages")
	pair.code.addDirectory("packages/local")
	pair.code.addDirectory("packages/local/node_modules")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactsValidatesCombinedNamespaceLinks(t *testing.T) {
	pair := newProgramPair(t)
	pair.dependencies.addDirectory(".bin")
	pair.dependencies.addDirectory("tool")
	pair.dependencies.addFile("tool/index.js", []byte("export {}\n"), 0644)
	pair.dependencies.addLink(".bin/tool", "../tool/index.js")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}

	escaping := newProgramPair(t)
	escaping.dependencies.addLink("escape", "../../outside")
	if _, err := verifyProgramArtifacts(context.Background(), escaping.artifacts); err == nil {
		t.Fatal("verifyProgramArtifacts accepted an escaping dependency link")
	}

	dangling := newProgramPair(t)
	dangling.code.addFile("file", []byte("x"), 0644)
	dangling.code.addLink("safe", "file/../..")
	if _, err := verifyProgramArtifacts(context.Background(), dangling.artifacts); err != nil {
		t.Fatalf("verifyProgramArtifacts rejected a confined ENOTDIR link: %v", err)
	}
}

func TestProgramArtifactsAcceptsUnrelatedTypeScriptWithoutSidecars(t *testing.T) {
	pair := newProgramPair(t)
	pair.code.addFile("source.ts", []byte("export const value = 1\n"), 0644)
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactsAcceptsTypeScriptDeclarationLocator(t *testing.T) {
	pair := newProgramPair(t)
	pair.code.addFile("source.ts", []byte("export const build = {}\n"), 0644)
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
	pair.code.setFile("helmr/declarations.json", locatorRaw)
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

type testProgramPair struct {
	artifacts    programArtifacts
	code         *memoryArtifact
	dependencies *memoryArtifact
}

func newProgramPair(t *testing.T) *testProgramPair {
	t.Helper()
	dependencyDigest := testDigest("dependency Artifact")
	lockfile := []byte("lockfileVersion = 1\n")
	programRaw, err := CanonicalProgramIndex(ProgramIndex{
		Architecture:         ArchitectureX8664,
		BuildContractVersion: ProgramBuildContractVersion,
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
		DependenciesDigest: dependencyDigest,
		FormatVersion:      ProgramIndexFormatVersion,
		Manager: ProgramManager{
			CapsuleDigest: testDigest("manager capsule"),
			Name:          PackageManagerBun,
			Version:       "1.3.10",
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

	code := newMemoryArtifact()
	code.addDirectory("helmr")
	code.addDirectory("node_modules")
	code.addFile("helmr/program.json", programRaw, 0644)
	code.addFile("helmr/declarations.json", locatorRaw, 0644)
	code.addFile("helmr/entry.mjs", []byte(ProgramEntry), 0644)
	code.addFile("build.js", []byte("export const build = {}\n"), 0644)
	code.addFile("package.json", []byte(`{"packageManager":"bun@1.3.10"}`), 0644)
	code.addFile("bun.lock", lockfile, 0644)
	dependencies := newMemoryArtifact()

	return &testProgramPair{
		artifacts: programArtifacts{
			Code: programArtifact{
				Digest:    testDigest("code Artifact"),
				SizeBytes: squashFSPhysicalAlign,
				MediaType: ProgramCodeArtifactMediaType,
				Reader:    code,
			},
			Dependencies: programArtifact{
				Digest:    dependencyDigest,
				SizeBytes: squashFSPhysicalAlign,
				MediaType: ProgramDependencyArtifactMediaType,
				Reader:    dependencies,
			},
		},
		code:         code,
		dependencies: dependencies,
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
