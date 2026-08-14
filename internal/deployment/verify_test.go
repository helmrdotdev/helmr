package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
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
		"Program index": func(program *testProgram) {
			program.artifact.files["helmr/declarations.json"] = []byte(
				`{"declarations":[],"formatVersion":0}`,
			)
		},
		"program entry": func(program *testProgram) {
			program.artifact.files["helmr/entry.mjs"] = []byte("process.exit(0)\n")
		},
		"reserved receipt path": func(program *testProgram) {
			program.artifact.addFile("helmr/receipt.json", []byte("{}"), 0o644)
		},
		"unknown Platform-owned path": func(program *testProgram) {
			program.artifact.addFile("helmr/modules.json", []byte("{}"), 0o644)
		},
		"evaluated config": func(program *testProgram) {
			program.artifact.files["helmr/config.json"] = []byte("{}")
		},
		"compiled module": func(program *testProgram) {
			for name := range program.artifact.files {
				if !strings.Contains(name, "/.helmr/modules/") ||
					!strings.HasSuffix(name, ".mjs") {
					continue
				}
				program.artifact.files[name] = []byte("export default null\n")
				return
			}
			t.Fatal("compiled module fixture is absent")
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

func TestProgramArtifactDoesNotInterpretProducerMetadata(t *testing.T) {
	program := newTestProgram(t)
	program.artifact.files["bun.lock"] = []byte("changed by lifecycle")
	program.artifact.files["package.json"] = []byte(
		`{"packageManager":"yarn@4.9.2"}`,
	)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatalf("verifyProgramArtifact rejected producer metadata: %v", err)
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

func TestProgramArtifactAcceptsLocalPackageInstallLayouts(t *testing.T) {
	for _, copied := range []bool{false, true} {
		name := "symlinked"
		if copied {
			name = "copied"
		}
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			program.artifact.addDirectory("packages")
			program.artifact.addDirectory("packages/local")
			program.artifact.addFile(
				"packages/local/package.json",
				[]byte(`{"name":"@example/local"}`),
				0644,
			)
			program.artifact.addDirectory("node_modules/@example")
			if copied {
				program.artifact.addDirectory("node_modules/@example/local")
				program.artifact.addFile(
					"node_modules/@example/local/package.json",
					[]byte(`{"name":"@example/local"}`),
					0644,
				)
			} else {
				program.artifact.addLink(
					"node_modules/@example/local",
					"../../packages/local",
				)
			}
			program.manifest.LocalPackages = []ProgramLocalPackage{{
				InstalledRoot: "node_modules/@example/local",
				Name:          "@example/local",
				SourceRoot:    "packages/local",
			}}
			program.refreshManifest(t)
			if _, err := verifyProgramArtifact(
				context.Background(),
				program.descriptor,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramArtifactBindsExternalDependencyResolution(t *testing.T) {
	newExternalProgram := func(t *testing.T, target string) *testProgram {
		t.Helper()
		program := newTestProgram(t)
		program.artifact.addDirectory("node_modules/.pnpm")
		program.artifact.addDirectory("node_modules/.pnpm/registry-package")
		program.artifact.addFile(
			"node_modules/.pnpm/registry-package/index.mjs",
			[]byte("export const value = true\n"),
			0644,
		)
		program.artifact.addLink("node_modules/registry-package", target)
		program.manifest.ExternalEdges = []ProgramExternalEdge{{
			Importer:     "tasks/build.ts",
			Kind:         "import-statement",
			LogicalPath:  "node_modules/registry-package/index.mjs",
			ResolvedPath: "node_modules/.pnpm/registry-package/index.mjs",
			RuntimePath:  "/opt/helmr/program/node_modules/registry-package/index.mjs",
			Specifier:    "registry-package",
		}}
		program.refreshManifest(t)
		return program
	}

	valid := newExternalProgram(t, ".pnpm/registry-package")
	if _, err := verifyProgramArtifact(context.Background(), valid.descriptor); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*testProgram){
		"missing": func(program *testProgram) {
			program.manifest.ExternalEdges[0].LogicalPath =
				"node_modules/missing/index.mjs"
			program.manifest.ExternalEdges[0].RuntimePath =
				"/opt/helmr/program/node_modules/missing/index.mjs"
		},
		"broken": func(program *testProgram) {
			program.artifact.mutate("node_modules/registry-package", func(entry *artifactEntry) {
				entry.LinkTarget = ".pnpm/missing"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
		"misdirected": func(program *testProgram) {
			program.artifact.addDirectory("node_modules/.pnpm/other")
			program.artifact.addFile(
				"node_modules/.pnpm/other/index.mjs",
				[]byte("export const value = false\n"),
				0644,
			)
			program.artifact.mutate("node_modules/registry-package", func(entry *artifactEntry) {
				entry.LinkTarget = ".pnpm/other"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
		"escaping": func(program *testProgram) {
			program.artifact.mutate("node_modules/registry-package", func(entry *artifactEntry) {
				entry.LinkTarget = "../../outside"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			program := newExternalProgram(t, ".pnpm/registry-package")
			mutate(program)
			program.refreshManifest(t)
			if _, err := verifyProgramArtifact(
				context.Background(),
				program.descriptor,
			); err == nil {
				t.Fatal("verifyProgramArtifact returned nil error")
			}
		})
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

type testProgram struct {
	descriptor artifactInput
	artifact   *memoryArtifact
	manifest   ProgramManifest
}

func newTestProgram(t *testing.T) *testProgram {
	t.Helper()
	lockfile := []byte("lockfileVersion = 1\n")
	configSourceRaw := []byte(
		`import { defineConfig } from "@helmr/sdk"; export default defineConfig({ dirs: ["tasks"] });`,
	)
	configRaw := []byte(
		`{"dirs":["tasks"],"ignorePatterns":[]}`,
	)
	sourcePath := "tasks/build.ts"
	sourceRaw := []byte("export const build = task({ id: \"build\" })\n")
	modulePath := generatedDeclarationModulePath(sourcePath)
	moduleRaw := []byte("export const build = {}\n")
	sourceMapRaw, err := jsoncanon.Transform([]byte(
		`{"mappings":"AAAA","names":[],"sources":["file:///opt/helmr/program/tasks/build.ts"],"version":3}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	programRaw, err := CanonicalProgramIndex(ProgramIndex{
		Architecture:       ArchitectureX8664,
		ConfigResultDigest: testDigest(string(configRaw)),
		Declarations: []ProgramIndexDeclaration{{
			Kind:       DefinitionKindTask,
			DeclaredID: "build",
			Task: &TaskManifest{
				Payload: SchemaManifest{Kind: SchemaKindNone},
				Run: RunManifest{
					Queue:         "task/build",
					MaxDurationMs: 900000,
					Retry:         RetryManifest{Enabled: false},
				},
			},
			Locator: &ProgramLocator{
				ExportName: "build",
				ModulePath: modulePath,
				Slot:       DeclarationSlotHandler,
			},
		}},
		Queues: []QueueInput{{
			Name: "task/build",
		}},
		RuntimeContract: RuntimeContract,
		RuntimeDigest:   "sha256:" + strings.Repeat("f", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := ProgramManifest{
		FormatVersion: ProgramManifestFormatVersion,
		Config: ProgramPathDigest{
			Digest: testDigest(string(configRaw)),
			Path:   "helmr/config.json",
		},
		ExternalEdges: []ProgramExternalEdge{},
		LocalPackages: []ProgramLocalPackage{},
		Modules: []ProgramModule{{
			ModuleDigest:    testDigest(string(moduleRaw)),
			ModulePath:      modulePath,
			SourceMapDigest: testDigest(string(sourceMapRaw)),
			SourceMapPath:   modulePath + ".map",
			SourcePath:      sourcePath,
		}},
		ProgramIndexDigest: testDigest(string(programRaw)),
	}
	manifestRaw, err := canonicalProgramManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	artifact := newMemoryArtifact()
	artifact.addDirectory("helmr")
	artifact.addDirectory("node_modules")
	artifact.addDirectory("tasks")
	artifact.addDirectory("tasks/.helmr")
	artifact.addDirectory("tasks/.helmr/modules")
	artifact.addFile("helmr/program-manifest.json", manifestRaw, 0644)
	artifact.addFile("helmr/config.json", configRaw, 0644)
	artifact.addFile("helmr/declarations.json", programRaw, 0644)
	artifact.addFile("helmr/entry.mjs", []byte(ProgramEntry), 0644)
	artifact.addFile(modulePath, moduleRaw, 0644)
	artifact.addFile(modulePath+".map", sourceMapRaw, 0644)
	artifact.addFile(sourcePath, sourceRaw, 0644)
	artifact.addFile("helmr.config.ts", configSourceRaw, 0644)
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
		manifest: manifest,
	}
}

func (program *testProgram) refreshManifest(t *testing.T) {
	t.Helper()
	raw, err := canonicalProgramManifest(program.manifest)
	if err != nil {
		t.Fatal(err)
	}
	program.artifact.files["helmr/program-manifest.json"] = raw
	program.artifact.mutate("helmr/program-manifest.json", func(entry *artifactEntry) {
		entry.SizeBytes = int64(len(raw))
	})
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
