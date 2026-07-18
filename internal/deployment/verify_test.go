package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
)

func TestProgramArtifactsAcceptsMinimalPair(t *testing.T) {
	pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
	verified, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if verified == nil || verified.Index().Declarations[0].DeclaredID != "build" {
		t.Fatalf("verified = %#v", verified)
	}
}

func TestProgramArtifactsRejectsIncompleteOrDivergentPair(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testProgramPair)
	}{
		{
			name: "dependency media type",
			mutate: func(pair *testProgramPair) {
				pair.artifacts.Dependencies.MediaType += "; charset=binary"
			},
		},
		{
			name: "descriptor size",
			mutate: func(pair *testProgramPair) {
				pair.artifacts.Code.SizeBytes = maxJSONSafeInteger + 1
			},
		},
		{
			name: "program dependency size",
			mutate: func(pair *testProgramPair) {
				pair.artifacts.Dependencies.SizeBytes++
			},
		},
		{
			name: "code hardlink",
			mutate: func(pair *testProgramPair) {
				pair.code.mutate("package.json", func(entry *artifactEntry) {
					entry.LinkCount = 2
				})
			},
		},
		{
			name: "nonempty code mountpoint",
			mutate: func(pair *testProgramPair) {
				pair.code.addFile("node_modules/unlisted.js", []byte("x"), 0644)
			},
		},
		{
			name: "missing directory parent",
			mutate: func(pair *testProgramPair) {
				pair.code.addFile("missing/file.js", []byte("x"), 0644)
			},
		},
		{
			name: "extra helmr file",
			mutate: func(pair *testProgramPair) {
				pair.code.addFile("helmr/extra.mjs", []byte("x"), 0644)
			},
		},
		{
			name: "dangling source link",
			mutate: func(pair *testProgramPair) {
				pair.code.addLink("current", "missing")
			},
		},
		{
			name: "source link into dependency artifact",
			mutate: func(pair *testProgramPair) {
				pair.code.addLink("external", "node_modules")
			},
		},
		{
			name: "missing component before parent traversal",
			mutate: func(pair *testProgramPair) {
				pair.code.addDirectory("assets")
				pair.code.addFile("assets/data.txt", []byte("value"), 0644)
				pair.code.addLink("current", "missing/../assets/data.txt")
			},
		},
		{
			name: "source link cycle",
			mutate: func(pair *testProgramPair) {
				pair.code.addLink("a", "b")
				pair.code.addLink("b", "a")
			},
		},
		{
			name: "symlink package manifest",
			mutate: func(pair *testProgramPair) {
				pair.code.addDirectory("nested")
				pair.code.addLink("nested/package.json", "../package.json")
			},
		},
		{
			name: "directory package manifest",
			mutate: func(pair *testProgramPair) {
				pair.code.addDirectory("nested")
				pair.code.addDirectory("nested/package.json")
			},
		},
		{
			name: "unowned dependency file",
			mutate: func(pair *testProgramPair) {
				pair.dependencies.addFile("unlisted.js", []byte("x"), 0644)
			},
		},
		{
			name: "short document read",
			mutate: func(pair *testProgramPair) {
				pair.code.files["helmr/modules.json"] = []byte{}
			},
		},
		{
			name: "entry module size",
			mutate: func(pair *testProgramPair) {
				pair.code.mutate("helmr/entry.mjs", func(entry *artifactEntry) {
					entry.SizeBytes = maxProgramFileSizeBytes + 1
				})
			},
		},
		{
			name: "lockfile size",
			mutate: func(pair *testProgramPair) {
				pair.code.mutate("bun.lock", func(entry *artifactEntry) {
					entry.SizeBytes = maxLockfileBytes + 1
				})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
			test.mutate(pair)
			verified, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
			if err == nil || verified != nil {
				t.Fatalf("verifyProgramArtifacts() = %#v, %v", verified, err)
			}
		})
	}
}

func TestProgramArtifactsRejectsRootManifestPolicy(t *testing.T) {
	for _, manifest := range []string{
		`{"packageManager":"npm@10.9.2"}`,
		`{"packageManager":"bun@1.3.10","scripts":{"prepare":"node generate.js"}}`,
	} {
		t.Run(manifest, func(t *testing.T) {
			pair := newMinimalProgramPair(t, manifest)
			verified, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
			if err == nil || verified != nil {
				t.Fatalf("verifyProgramArtifacts() = %#v, %v", verified, err)
			}
		})
	}
}

func TestProgramArtifactsAcceptsConfinedSourceLink(t *testing.T) {
	pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
	pair.code.addFile("assets/data.txt", []byte("value"), 0644)
	pair.code.addDirectory("assets")
	pair.code.addLink("current", "assets/data.txt")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactsAppliesOnlyPackageScopeContractToNestedManifest(t *testing.T) {
	pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
	pair.code.addDirectory("nested")
	pair.code.addFile("nested/package.json", []byte(`{
		"bin":null,
		"name":null,
		"packageManager":null,
		"scripts":{"prepare":"node generate.js"},
		"type":"module",
		"version":null
	}`), 0644)
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}

	pair = newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
	pair.code.addDirectory("nested")
	pair.code.addFile("nested/package.json", []byte(`{"type":"dual"}`), 0644)
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err == nil {
		t.Fatal("nested package manifest with invalid type was accepted")
	}
}

func TestProgramArtifactsAcceptsSourceLinksEndingInDirectoryTraversal(t *testing.T) {
	pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
	pair.code.addDirectory("a")
	pair.code.addDirectory("a/b")
	pair.code.addLink("a/current", ".")
	pair.code.addLink("a/b/parent", "..")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err != nil {
		t.Fatal(err)
	}
}

func TestProgramArtifactsAcceptsCompletePair(t *testing.T) {
	pair := newCompleteProgramPair(t)
	verified, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
	if err != nil {
		t.Fatal(err)
	}
	if verified == nil || len(verified.modules.Modules) != 1 ||
		len(verified.graph.LocalPackages) != 2 ||
		len(verified.graph.RegistryPackages) != 1 {
		t.Fatalf("verified = %#v", verified)
	}
}

func TestProgramArtifactsRejectsRegistryLinkIntoCodeArtifact(t *testing.T) {
	pair := newCompleteProgramPair(t)
	pair.dependencies.addLink("tool/source", "../../packages/local")
	if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err == nil {
		t.Fatal("registry link into code Artifact was accepted")
	}
}

func TestArtifactResourceBounds(t *testing.T) {
	entry := func(path string, size int64, inode uint64) artifactEntry {
		return artifactEntry{
			Path:        path,
			Kind:        artifactEntryRegular,
			Form:        squashFSBasicRegularForm,
			Mode:        0644,
			SizeBytes:   size,
			XattrIndex:  squashFSInvalidXattr,
			Inode:       inode,
			InodeNumber: uint32(inode),
			LinkCount:   1,
		}
	}
	root := artifactEntry{
		Path:        ".",
		Kind:        artifactEntryDirectory,
		Form:        squashFSBasicDirectoryForm,
		Mode:        0755,
		XattrIndex:  squashFSInvalidXattr,
		Inode:       1,
		InodeNumber: 1,
	}
	reader := func(entries []artifactEntry) *memoryArtifact {
		filesystem := exactTestFilesystem()
		filesystem.InodeCount = uint32(len(entries))
		return &memoryArtifact{
			files:      map[string][]byte{},
			entries:    entries,
			nextInode:  uint64(len(entries) + 1),
			filesystem: filesystem,
		}
	}

	if _, err := inspectArtifact(
		context.Background(),
		reader([]artifactEntry{
			root,
			entry("a", maxArtifactFileSize, 2),
			entry("b", maxArtifactFileSize, 3),
		}),
		codeArtifact,
		maxCodeLogicalBytes,
		4096,
	); err != nil {
		t.Fatalf("exact code logical bound: %v", err)
	}
	for name, test := range map[string]struct {
		entries []artifactEntry
		role    artifactRole
		limit   int64
	}{
		"file": {
			entries: []artifactEntry{root, entry("a", maxArtifactFileSize+1, 2)},
			role:    codeArtifact,
			limit:   maxCodeLogicalBytes,
		},
		"code aggregate": {
			entries: []artifactEntry{
				root,
				entry("a", maxArtifactFileSize, 2),
				entry("b", maxArtifactFileSize, 3),
				entry("c", 1, 4),
			},
			role:  codeArtifact,
			limit: maxCodeLogicalBytes,
		},
		"dependency aggregate": {
			entries: append(
				[]artifactEntry{root},
				func() []artifactEntry {
					entries := make([]artifactEntry, 9)
					for position := range entries {
						entries[position] = entry(
							fmt.Sprintf("p%d", position),
							maxArtifactFileSize,
							uint64(position+2),
						)
					}
					return entries
				}()...,
			),
			role:  dependencyArtifact,
			limit: maxDependencyLogicalBytes,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inspectArtifact(
				context.Background(),
				reader(test.entries),
				test.role,
				test.limit,
				4096,
			); err == nil {
				t.Fatal("inspectArtifact returned nil error")
			}
		})
	}

	tooMany := make([]artifactEntry, maxArtifactEntries+1)
	if _, err := inspectArtifact(
		context.Background(),
		reader(tooMany),
		codeArtifact,
		maxCodeLogicalBytes,
		4096,
	); err == nil {
		t.Fatal("inspectArtifact accepted too many entries")
	}
}

func TestArtifactPhysicalBounds(t *testing.T) {
	reader := newMemoryArtifact()
	for _, test := range []struct {
		name  string
		role  artifactRole
		limit int64
	}{
		{
			name:  "code",
			role:  codeArtifact,
			limit: maxCodePhysicalBytes,
		},
		{
			name:  "dependency",
			role:  dependencyArtifact,
			limit: maxDependencyPhysicalBytes,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mediaType := ProgramCodeArtifactMediaType
			if test.role == dependencyArtifact {
				mediaType = ProgramDependencyArtifactMediaType
			}
			artifact := programArtifact{
				Digest:    testDigest(test.name),
				SizeBytes: test.limit,
				MediaType: mediaType,
				Reader:    reader,
			}
			if err := validateArtifactDescriptor(artifact, test.role); err != nil {
				t.Fatalf("exact physical bound: %v", err)
			}
			artifact.SizeBytes++
			if err := validateArtifactDescriptor(artifact, test.role); err == nil {
				t.Fatal("descriptor above physical bound was accepted")
			}
		})
	}

	dependency := programArtifact{
		Digest:    testDigest("dependency above code limit"),
		SizeBytes: maxCodePhysicalBytes + 1,
		MediaType: ProgramDependencyArtifactMediaType,
		Reader:    reader,
	}
	if err := validateArtifactDescriptor(dependency, dependencyArtifact); err != nil {
		t.Fatalf("dependency above code physical bound: %v", err)
	}
}

func TestProgramArtifactPhysicalPolicyWiring(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*programArtifacts)
	}{
		{
			name: "code",
			mutate: func(artifacts *programArtifacts) {
				artifacts.Code.SizeBytes = maxCodePhysicalBytes + 1
			},
		},
		{
			name: "dependency",
			mutate: func(artifacts *programArtifacts) {
				artifacts.Dependencies.SizeBytes = maxDependencyPhysicalBytes + 1
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pair := newCompleteProgramPair(t)
			test.mutate(&pair.artifacts)
			if _, err := verifyProgramArtifacts(context.Background(), pair.artifacts); err == nil {
				t.Fatal("Program accepted an Artifact above its role-specific physical bound")
			}
		})
	}
}

func TestArtifactFilesystemRejectsForbiddenFacts(t *testing.T) {
	for name, mutate := range map[string]func(*artifactFilesystem){
		"fragments": func(filesystem *artifactFilesystem) {
			filesystem.FragmentCount = 1
		},
		"duplicate packing": func(filesystem *artifactFilesystem) {
			filesystem.Flags ^= 1
		},
		"fragment references": func(filesystem *artifactFilesystem) {
			filesystem.HasFragmentRefs = true
		},
		"overlapping data": func(filesystem *artifactFilesystem) {
			filesystem.HasOverlappingData = true
		},
		"export table": func(filesystem *artifactFilesystem) {
			filesystem.ExportTableStart = 0
		},
		"xattrs": func(filesystem *artifactFilesystem) {
			filesystem.XattrIDTableStart = 0
		},
	} {
		t.Run(name, func(t *testing.T) {
			reader := newMemoryArtifact()
			mutate(&reader.filesystem)
			if _, err := inspectArtifact(
				context.Background(),
				reader,
				codeArtifact,
				maxCodeLogicalBytes,
				4096,
			); err == nil {
				t.Fatal("forbidden filesystem fact was accepted")
			}
		})
	}
}

func TestInspectArtifactRejectsRepeatedInodeReference(t *testing.T) {
	reader := newMemoryArtifact()
	reader.addFile("a", []byte("content"), 0644)
	repeated := reader.entries[len(reader.entries)-1]
	repeated.Path = "b"
	reader.entries = append(reader.entries, repeated)
	_, err := inspectArtifact(
		context.Background(),
		reader,
		codeArtifact,
		maxCodeLogicalBytes,
		4096,
	)
	if err == nil || !strings.Contains(err.Error(), "share inode reference") {
		t.Fatalf("repeated inode reference error = %v", err)
	}
}

func TestInspectArtifactRejectsInodeNumberAboveSuperblockCount(t *testing.T) {
	reader := newMemoryArtifact()
	reader.addFile("file", []byte("content"), 0644)
	reader.mutate("file", func(entry *artifactEntry) {
		entry.InodeNumber = reader.filesystem.InodeCount + 1
	})
	if _, err := inspectArtifact(
		context.Background(),
		reader,
		codeArtifact,
		maxCodeLogicalBytes,
		4096,
	); err == nil {
		t.Fatal("inode number above the superblock count was accepted")
	}
}

func TestArtifactAggregateNameBound(t *testing.T) {
	entry := artifactEntry{
		Path:       "path",
		LinkTarget: strings.Repeat("x", maxSymlinkTargetBytes),
	}
	charge := int64(len(entry.Path) + len(entry.LinkTarget))
	total, err := chargeArtifactNameBytes(maxArtifactNameBytes-charge, entry)
	if err != nil {
		t.Fatalf("exact aggregate name bound: %v", err)
	}
	if total != maxArtifactNameBytes {
		t.Fatalf("total = %d, want %d", total, maxArtifactNameBytes)
	}
	if _, err := chargeArtifactNameBytes(maxArtifactNameBytes-charge+1, entry); err == nil {
		t.Fatal("aggregate name bytes above bound were accepted")
	}
}

func TestArtifactDepthBounds(t *testing.T) {
	for _, test := range []struct {
		name string
		role artifactRole
		path func(string) string
	}{
		{name: "code", role: codeArtifact, path: absoluteCode},
		{name: "dependency", role: dependencyArtifact, path: absoluteDependency},
	} {
		t.Run(test.name, func(t *testing.T) {
			atLimit := strings.TrimSuffix(strings.Repeat("a/", maxArtifactDepth), "/")
			overLimit := atLimit + "/a"
			if err := validateArtifactPath(atLimit, test.role); err != nil {
				t.Fatalf("validateArtifactPath at limit: %v", err)
			}
			if err := validateResolvedAbsolute(test.path(atLimit)); err != nil {
				t.Fatalf("validateResolvedAbsolute at limit: %v", err)
			}
			if err := validateArtifactPath(overLimit, test.role); err == nil {
				t.Fatal("validateArtifactPath accepted path over depth limit")
			}
			if err := validateResolvedAbsolute(test.path(overLimit)); err == nil {
				t.Fatal("validateResolvedAbsolute accepted path over depth limit")
			}
		})
	}
}

func TestMountedPathByteBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		role  artifactRole
		mount string
	}{
		{name: "code", role: codeArtifact, mount: programMountPath},
		{name: "dependency", role: dependencyArtifact, mount: dependencyMountPath},
		{name: "runtime", role: runtimeArtifact, mount: runtimeMountPath},
	} {
		t.Run(test.name, func(t *testing.T) {
			atLimit := testPathOfLength(maxMountedPackagePath - len(test.mount) - 2)
			if err := validateArtifactPath(atLimit, test.role); err != nil {
				t.Fatalf("validateArtifactPath at mounted byte limit: %v", err)
			}
			overLimit := atLimit + "a"
			if err := validateArtifactPath(overLimit, test.role); err == nil {
				t.Fatal("validateArtifactPath accepted path over mounted byte limit")
			}
		})
	}
}

func TestSymlinkHopBound(t *testing.T) {
	for _, count := range []int{maxSymlinkHops, maxSymlinkHops + 1} {
		t.Run(fmt.Sprintf("%d", count), func(t *testing.T) {
			pair := newMinimalProgramPair(t, `{"packageManager":"bun@1.3.10"}`)
			for position := 0; position < count; position++ {
				target := "package.json"
				if position+1 < count {
					target = fmt.Sprintf("link-%02d", position+1)
				}
				pair.code.addLink(fmt.Sprintf("link-%02d", position), target)
			}
			_, err := verifyProgramArtifacts(context.Background(), pair.artifacts)
			if count == maxSymlinkHops && err != nil {
				t.Fatal(err)
			}
			if count > maxSymlinkHops && err == nil {
				t.Fatal("link walk over hop bound was accepted")
			}
		})
	}
}

func TestPackageManifestAggregateBound(t *testing.T) {
	entries := make([]artifactEntry, maxPackageJSONBytes/maxPackageManifestSizeBytes)
	for position := range entries {
		entries[position] = artifactEntry{
			Path:      fmt.Sprintf("p%d/package.json", position),
			Kind:      artifactEntryRegular,
			SizeBytes: maxPackageManifestSizeBytes,
		}
	}
	artifact := &inspectedArtifact{ordered: entries}
	if _, err := manifestEntries(artifact, "helmr", "code"); err != nil {
		t.Fatalf("manifestEntries at limit: %v", err)
	}
	artifact.ordered = append(artifact.ordered, artifactEntry{
		Path:      "overflow/package.json",
		Kind:      artifactEntryRegular,
		SizeBytes: 1,
	})
	if _, err := manifestEntries(artifact, "helmr", "code"); err == nil {
		t.Fatal("manifestEntries accepted aggregate over limit")
	}
}

func TestPairVerifierDerivesCanonicalBinLink(t *testing.T) {
	root := "."
	installPath := "tool"
	dependencies := newMemoryArtifact()
	dependencies.addDirectory(installPath)
	dependencies.addDirectory(installPath + "/bin")
	dependencies.addFile(installPath+"/bin/cli.py", []byte("print('ok')\n"), 0755)
	code, err := inspectArtifact(
		context.Background(),
		newMemoryArtifact(),
		codeArtifact,
		maxCodeLogicalBytes,
		4096,
	)
	if err != nil {
		t.Fatal(err)
	}
	dependency, err := inspectArtifact(
		context.Background(),
		dependencies,
		dependencyArtifact,
		maxDependencyLogicalBytes,
		4096,
	)
	if err != nil {
		t.Fatal(err)
	}

	verifier := pairVerifier{
		ctx:  context.Background(),
		code: code,
		deps: dependency,
		graph: PackageGraph{
			LocalPackages: []LocalPackage{{Path: root}},
			RegistryPackages: []RegistryPackage{{
				InstallPath: installPath,
				Name:        "tool",
				Version:     "1.0.0",
			}},
			Resolutions: []PackageResolution{{
				Dependency:   "tool",
				From:         PackageEndpoint{Kind: PackageKindLocal, Path: &root},
				Relationship: PackageRelationshipProduction,
				To:           PackageEndpoint{Kind: PackageKindRegistry, InstallPath: &installPath},
			}},
		},
		codeManifests: map[string]packageManifest{
			"package.json": {Bins: map[string]string{}},
		},
		depManifests: map[string]packageManifest{
			"tool/package.json": {
				Bins: map[string]string{"tool": "bin/cli.py"},
			},
		},
	}
	verifier.indexGraph()
	if err := verifier.deriveTopology(); err != nil {
		t.Fatal(err)
	}

	want := "../tool/bin/cli.py"
	if got := verifier.binLinks[".bin/tool"]; got != want {
		t.Fatalf("bin link = %q, want %q", got, want)
	}

	target := dependency.entries[installPath+"/bin/cli.py"]
	target.Mode = 0644
	dependency.entries[target.Path] = target
	if err := verifier.deriveTopology(); err == nil {
		t.Fatal("deriveTopology accepted a non-executable bin target")
	}
}

func TestVerifyDependencyPathRejectsUnlistedRegistryDependency(t *testing.T) {
	verifier := pairVerifier{
		graph: PackageGraph{
			RegistryPackages: []RegistryPackage{
				{InstallPath: "parent"},
				{InstallPath: "parent/node_modules/child"},
			},
		},
		depLinks: map[string]string{},
		binLinks: map[string]string{},
		depDirs:  map[string]struct{}{},
	}
	verifier.indexGraph()
	unlisted := artifactEntry{
		Path: "parent/node_modules/unlisted",
		Kind: artifactEntryDirectory,
	}
	if err := verifier.verifyDependencyPath(unlisted); err == nil {
		t.Fatal("verifyDependencyPath returned nil error")
	}

	nestedUnlisted := artifactEntry{
		Path: "parent/lib/node_modules/unlisted",
		Kind: artifactEntryDirectory,
	}
	if err := verifier.verifyDependencyPath(nestedUnlisted); err == nil {
		t.Fatal("verifyDependencyPath returned nil error for nested unlisted dependency")
	}

	childAsset := artifactEntry{
		Path: "parent/node_modules/child/asset.js",
		Kind: artifactEntryRegular,
	}
	if err := verifier.verifyDependencyPath(childAsset); err != nil {
		t.Fatalf("verifyDependencyPath(child asset): %v", err)
	}
}

type testProgramPair struct {
	artifacts    programArtifacts
	code         *memoryArtifact
	dependencies *memoryArtifact
}

func newMinimalProgramPair(t *testing.T, rootManifest string) *testProgramPair {
	t.Helper()
	runtimeDigest := testDigest("runtime")
	dependencyDigest := testDigest("dependency Artifact")
	lockfile := []byte("lockfileVersion = 1\n")
	manifest := []byte(rootManifest)

	graphRaw, err := CanonicalPackageGraph(PackageGraph{
		FormatVersion: PackageGraphFormatVersion,
		LocalPackages: []LocalPackage{{
			ManifestDigest: digestBytes(manifest),
			Path:           ".",
		}},
		RegistryPackages: []RegistryPackage{},
		Resolutions:      []PackageResolution{},
	})
	if err != nil {
		t.Fatal(err)
	}
	localDigest, err := LocalManifestsDigest(LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries: []LocalManifestEntry{{
			ManifestDigest: digestBytes(manifest),
			Path:           ".",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	dependencyRaw, err := CanonicalDependencyIndex(DependencyIndex{
		FormatVersion:         DependencyIndexFormatVersion,
		DependencyToolsDigest: testDigest("dependency tools"),
		PackageManager: PackageManager{
			Name:    PackageManagerBun,
			Version: "1.3.10",
		},
		Lockfile: DependencyLockfile{
			Name:   "bun.lock",
			Digest: digestBytes(lockfile),
		},
		LocalManifestsDigest:  "sha256:" + hex.EncodeToString(localDigest[:]),
		PackageGraphDigest:    digestBytes(graphRaw),
		PackageGraphSizeBytes: int64(len(graphRaw)),
		MaterializerVersion:   DependencyMaterializerVersion,
		RuntimeDigest:         runtimeDigest,
		Architecture:          ArchitectureX8664,
	})
	if err != nil {
		t.Fatal(err)
	}
	moduleRaw, err := CanonicalModuleMap(ModuleMap{
		FormatVersion: ModuleMapFormatVersion,
		Modules:       []Module{},
		Transformer:   TypeScriptTransformer,
	})
	if err != nil {
		t.Fatal(err)
	}
	programRaw, err := CanonicalProgramIndex(ProgramIndex{
		FormatVersion:     ProgramIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
		RuntimeDigest:     runtimeDigest,
		Architecture:      ArchitectureX8664,
		Dependencies: ProgramDependencies{
			Digest:    dependencyDigest,
			SizeBytes: 4096,
			MediaType: ProgramDependencyArtifactMediaType,
		},
		PackageGraph: ProgramFile{
			Digest:    digestBytes(graphRaw),
			SizeBytes: int64(len(graphRaw)),
		},
		Modules: ProgramFile{
			Digest:    digestBytes(moduleRaw),
			SizeBytes: int64(len(moduleRaw)),
		},
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	code := newMemoryArtifact()
	code.addDirectory("helmr")
	code.addDirectory("node_modules")
	code.addFile("helmr/program.json", programRaw, 0644)
	code.addFile("helmr/modules.json", moduleRaw, 0644)
	code.addFile("helmr/entry.mjs", []byte("export const helmrProgram={formatVersion:0,declarations:[]}\n"), 0644)
	code.addFile("package.json", manifest, 0644)
	code.addFile("bun.lock", lockfile, 0644)

	dependencies := newMemoryArtifact()
	dependencies.addDirectory(".helmr")
	dependencies.addDirectory(".helmr/views")
	dependencies.addFile(".helmr/dependencies.json", dependencyRaw, 0644)
	dependencies.addFile(".helmr/package-graph.json", graphRaw, 0644)

	return &testProgramPair{
		artifacts: programArtifacts{
			Code: programArtifact{
				Digest:    testDigest("code Artifact"),
				SizeBytes: 4096,
				MediaType: ProgramCodeArtifactMediaType,
				Reader:    code,
			},
			Dependencies: programArtifact{
				Digest:    dependencyDigest,
				SizeBytes: 4096,
				MediaType: ProgramDependencyArtifactMediaType,
				Reader:    dependencies,
			},
		},
		code:         code,
		dependencies: dependencies,
	}
}

func newCompleteProgramPair(t *testing.T) *testProgramPair {
	t.Helper()
	runtimeDigest := testDigest("runtime")
	dependencyDigest := testDigest("complete dependency Artifact")
	lockfile := []byte("lockfileVersion = 1\n")
	rootManifest := []byte(`{"packageManager":"bun@1.3.10"}`)
	localManifest := []byte(
		`{"bin":{"local":"bin/cli.js"},"name":"@test/local","packageManager":null,"type":"module","version":"1.0.0"}`,
	)
	registryManifest := []byte(
		`{"bin":{"tool":"bin/cli.js"},"name":"tool","packageManager":null,"scripts":{"prepare":"node generate.js"},"version":"1.0.0"}`,
	)
	localName := "@test/local"
	localVersion := "1.0.0"
	localPath := "packages/local"
	viewKey := localPackageViewKey(localPath)
	registryPath := "tool"
	sourcePath := "packages/local/src/run.ts"
	source := []byte("export const value: number = 1\n")
	compiled := []byte("export const value = 1;\n")
	codePath := moduleCodePath(sourcePath, ModuleFormatESM)
	rootPath := "."

	graph := PackageGraph{
		FormatVersion: PackageGraphFormatVersion,
		LocalPackages: []LocalPackage{
			{ManifestDigest: digestBytes(rootManifest), Path: rootPath},
			{
				ManifestDigest: digestBytes(localManifest),
				Name:           &localName,
				Path:           localPath,
				Version:        &localVersion,
				ViewKey:        &viewKey,
			},
		},
		RegistryPackages: []RegistryPackage{{
			InstallPath: registryPath,
			Integrity:   "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512DigestBytes)),
			Name:        "tool",
			Version:     "1.0.0",
		}},
		Resolutions: []PackageResolution{
			{
				Dependency:   localName,
				From:         PackageEndpoint{Kind: PackageKindLocal, Path: &rootPath},
				Relationship: PackageRelationshipProduction,
				To:           PackageEndpoint{Kind: PackageKindLocal, Path: &localPath},
			},
			{
				Dependency:   "tool",
				From:         PackageEndpoint{Kind: PackageKindLocal, Path: &rootPath},
				Relationship: PackageRelationshipProduction,
				To: PackageEndpoint{
					Kind:        PackageKindRegistry,
					InstallPath: &registryPath,
				},
			},
			{
				Dependency:   "tool",
				From:         PackageEndpoint{Kind: PackageKindLocal, Path: &localPath},
				Relationship: PackageRelationshipProduction,
				To: PackageEndpoint{
					Kind:        PackageKindRegistry,
					InstallPath: &registryPath,
				},
			},
		},
	}
	graphRaw, err := CanonicalPackageGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	localDigest, err := LocalManifestsDigest(LocalManifests{
		FormatVersion: LocalManifestsFormatVersion,
		Entries: []LocalManifestEntry{
			{ManifestDigest: digestBytes(rootManifest), Path: rootPath},
			{ManifestDigest: digestBytes(localManifest), Path: localPath},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dependencyRaw, err := CanonicalDependencyIndex(DependencyIndex{
		FormatVersion:         DependencyIndexFormatVersion,
		DependencyToolsDigest: testDigest("dependency tools"),
		PackageManager: PackageManager{
			Name:    PackageManagerBun,
			Version: "1.3.10",
		},
		Lockfile:              DependencyLockfile{Name: "bun.lock", Digest: digestBytes(lockfile)},
		LocalManifestsDigest:  "sha256:" + hex.EncodeToString(localDigest[:]),
		PackageGraphDigest:    digestBytes(graphRaw),
		PackageGraphSizeBytes: int64(len(graphRaw)),
		MaterializerVersion:   DependencyMaterializerVersion,
		RuntimeDigest:         runtimeDigest,
		Architecture:          ArchitectureX8664,
	})
	if err != nil {
		t.Fatal(err)
	}
	moduleRaw, err := CanonicalModuleMap(ModuleMap{
		FormatVersion: ModuleMapFormatVersion,
		Modules: []Module{{
			CodeDigest:   digestBytes(compiled),
			CodePath:     codePath,
			Format:       ModuleFormatESM,
			Path:         sourcePath,
			SourceDigest: digestBytes(source),
		}},
		Transformer: TypeScriptTransformer,
	})
	if err != nil {
		t.Fatal(err)
	}
	programRaw, err := CanonicalProgramIndex(ProgramIndex{
		FormatVersion:     ProgramIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
		RuntimeDigest:     runtimeDigest,
		Architecture:      ArchitectureX8664,
		Dependencies: ProgramDependencies{
			Digest:    dependencyDigest,
			SizeBytes: 4096,
			MediaType: ProgramDependencyArtifactMediaType,
		},
		PackageGraph: ProgramFile{Digest: digestBytes(graphRaw), SizeBytes: int64(len(graphRaw))},
		Modules:      ProgramFile{Digest: digestBytes(moduleRaw), SizeBytes: int64(len(moduleRaw))},
		Declarations: []ProgramDeclaration{{
			Kind:       DeclarationKindTask,
			DeclaredID: "build",
			Slots:      []DeclarationSlot{DeclarationSlotHandler},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	code := newMemoryArtifact()
	for _, directory := range []string{
		"helmr",
		"helmr/files",
		"helmr/files/modules",
		"node_modules",
		"packages",
		localPath,
		localPath + "/bin",
		localPath + "/src",
	} {
		code.addDirectory(directory)
	}
	code.addFile("helmr/program.json", programRaw, 0644)
	code.addFile("helmr/modules.json", moduleRaw, 0644)
	code.addFile("helmr/entry.mjs", []byte("export const helmrProgram={formatVersion:0,declarations:[]}\n"), 0644)
	code.addFile(codePath, compiled, 0644)
	code.addFile("package.json", rootManifest, 0644)
	code.addFile("bun.lock", lockfile, 0644)
	code.addFile(localPath+"/package.json", localManifest, 0644)
	code.addFile(localPath+"/bin/cli.js", []byte("export {}\n"), 0755)
	code.addFile(sourcePath, source, 0644)
	localNodeModules := localPath + "/node_modules"
	code.addLink(
		localNodeModules,
		canonicalRelativeTarget(
			absoluteCode(localNodeModules),
			absoluteDependency(".helmr/views/"+viewKey),
		),
	)

	dependencies := newMemoryArtifact()
	for _, directory := range []string{
		".helmr",
		".helmr/views",
		".helmr/views/" + viewKey,
		".helmr/views/" + viewKey + "/.bin",
		".bin",
		"@test",
		registryPath,
		registryPath + "/bin",
	} {
		dependencies.addDirectory(directory)
	}
	dependencies.addFile(".helmr/dependencies.json", dependencyRaw, 0644)
	dependencies.addFile(".helmr/package-graph.json", graphRaw, 0644)
	dependencies.addFile(registryPath+"/package.json", registryManifest, 0644)
	dependencies.addFile(registryPath+"/bin/cli.js", []byte("export {}\n"), 0755)
	dependencies.addLink(
		localName,
		canonicalRelativeTarget(absoluteDependency(localName), absoluteCode(localPath)),
	)
	viewTool := ".helmr/views/" + viewKey + "/tool"
	dependencies.addLink(
		viewTool,
		canonicalRelativeTarget(absoluteDependency(viewTool), absoluteDependency(registryPath)),
	)
	dependencies.addLink(
		".bin/local",
		canonicalRelativeTarget(
			absoluteDependency(".bin/local"),
			absoluteCode(localPath+"/bin/cli.js"),
		),
	)
	dependencies.addLink(
		".bin/tool",
		canonicalRelativeTarget(
			absoluteDependency(".bin/tool"),
			absoluteDependency(registryPath+"/bin/cli.js"),
		),
	)
	dependencies.addLink(
		".helmr/views/"+viewKey+"/.bin/tool",
		canonicalRelativeTarget(
			absoluteDependency(".helmr/views/"+viewKey+"/.bin/tool"),
			absoluteDependency(registryPath+"/bin/cli.js"),
		),
	)

	return &testProgramPair{
		artifacts: programArtifacts{
			Code: programArtifact{
				Digest:    testDigest("complete code Artifact"),
				SizeBytes: 4096,
				MediaType: ProgramCodeArtifactMediaType,
				Reader:    code,
			},
			Dependencies: programArtifact{
				Digest:    dependencyDigest,
				SizeBytes: 4096,
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

func TestCanonicalRelativeTarget(t *testing.T) {
	tests := map[string]string{
		"/opt/helmr/program/packages/shared/node_modules": "../../node_modules/.helmr/views/view",
		"/opt/helmr/program/node_modules/alias":           "target",
	}
	targets := map[string]string{
		"/opt/helmr/program/packages/shared/node_modules": "/opt/helmr/program/node_modules/.helmr/views/view",
		"/opt/helmr/program/node_modules/alias":           "/opt/helmr/program/node_modules/target",
	}
	for link, want := range tests {
		if got := canonicalRelativeTarget(link, targets[link]); got != want {
			t.Errorf("canonicalRelativeTarget(%q) = %q, want %q", link, got, want)
		}
	}
}

func TestValidateSymlinkTargetRejectsInvalidTargets(t *testing.T) {
	for _, target := range []string{"", "/absolute", "a//b", "a\\b", "a\nb", strings.Repeat("a", 4096)} {
		if err := validateSymlinkTarget(target); err == nil {
			t.Errorf("validateSymlinkTarget(%q) returned nil", target)
		}
	}
}

func TestValidateSymlinkTargetAcceptsExactByteBound(t *testing.T) {
	target := strings.TrimSuffix(strings.Repeat(strings.Repeat("a", 255)+"/", 16), "/")
	if len(target) != maxSymlinkTargetBytes {
		t.Fatalf("target length = %d", len(target))
	}
	if err := validateSymlinkTarget(target); err != nil {
		t.Fatal(err)
	}
	if err := validateSymlinkTarget(target + "/a"); err == nil {
		t.Fatal("validateSymlinkTarget accepted target over byte limit")
	}
}

func testPathOfLength(length int) string {
	components := make([]string, 0, length/256+1)
	remaining := length
	for remaining > 0 {
		componentLength := min(remaining, maxPackagePathComponent)
		if remaining > componentLength {
			remaining--
		}
		components = append(components, strings.Repeat("a", componentLength))
		remaining -= componentLength
	}
	return strings.Join(components, "/")
}
