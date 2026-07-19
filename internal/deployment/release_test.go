package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestPrepareRuntimeReleaseIsCanonicalAndRetainsCapturedBytes(t *testing.T) {
	raw := []byte("one captured managed runtime")
	release := prepareRuntimeReleaseForTest(t, raw, nil)
	defer release.Close()

	catalog, err := ParseRuntimeCatalog(release.Catalog())
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.runtimes) != 1 || catalog.runtimes[0] != release.valid {
		t.Fatalf("runtime catalog = %#v", catalog.runtimes)
	}
	statement := release.Statement()
	if canonical, err := canonicalRuntimeReleaseStatement(
		release.catalog,
		catalog,
		release.lineage.Predecessor,
	); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(statement, canonical) {
		t.Fatal("runtime release statement is not canonical")
	}
	corpus, err := parseRuntimeVerifierCorpusManifest(
		release.Corpus(),
		authenticatedRuntimeCatalogForTest(t, catalog.runtimes),
		ArchitectureX8664,
	)
	if err != nil {
		t.Fatal(err)
	}
	if corpus.Valid.Descriptor != release.valid ||
		corpus.Valid.ExpectedIndex != release.index {
		t.Fatalf("runtime corpus = %#v", corpus)
	}
	var visited []byte
	if err := release.ForEachRuntime(
		context.Background(),
		func(descriptor RuntimeDescriptor, source io.Reader) error {
			if descriptor != release.valid {
				t.Fatalf("visited descriptor = %#v", descriptor)
			}
			value, err := io.ReadAll(source)
			visited = append(visited, value...)
			return err
		},
	); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(visited, raw) {
		t.Fatal("runtime release object does not equal the captured source")
	}
}

func TestToolchainReleaseStatementDeduplicatesSharedClosures(t *testing.T) {
	first := testToolchain(t)
	second := first
	second.Architecture = ArchitectureX8664
	firstDigest, err := StandardToolchainDigest(first)
	if err != nil {
		t.Fatal(err)
	}
	secondDigest, err := StandardToolchainDigest(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest {
		t.Fatal("fixture toolchains have identical semantic identities")
	}
	raw, err := CanonicalToolchainCatalog([]Toolchain{first, second})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseToolchainCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	statement, err := canonicalToolchainReleaseStatement(raw, catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	var document toolchainAttestationDocument
	if err := json.Unmarshal(statement, &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Subject) != 2 {
		t.Fatalf("standard-toolchain subjects = %#v", document.Subject)
	}
	catalogHash := sha256.Sum256(raw)
	if err := validateToolchainAttestationSubjects(
		document.Subject,
		catalogHash,
		catalog,
	); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeReleaseLineageIsClosedCanonicalAndTagBound(t *testing.T) {
	lineage := RuntimeReleaseLineage{
		FormatVersion: 0,
		Release:       "v1.2.3-rc.1",
		Predecessor: &RuntimeReleaseRef{
			Release:   "v1.2.2",
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 123,
		},
	}
	raw, err := CanonicalRuntimeReleaseLineage(lineage)
	if err != nil {
		t.Fatal(err)
	}
	if parsed, err := ParseRuntimeReleaseLineage(raw, lineage.Release); err != nil {
		t.Fatal(err)
	} else if !runtimeReleaseRefsEqual(parsed.Predecessor, lineage.Predecessor) ||
		parsed.Release != lineage.Release {
		t.Fatalf("runtime release lineage = %#v", parsed)
	}
	for name, divergent := range map[string][]byte{
		"noncanonical": append([]byte(" "), raw...),
		"trailing newline": append(
			append([]byte(nil), raw...),
			'\n',
		),
		"unknown": []byte(strings.Replace(
			string(raw),
			`"release":`,
			`"unknown":true,"release":`,
			1,
		)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseRuntimeReleaseLineage(
				divergent,
				lineage.Release,
			); err == nil {
				t.Fatal("runtime release lineage accepted shape drift")
			}
		})
	}
	if _, err := ParseRuntimeReleaseLineage(raw, "v1.2.4"); err == nil {
		t.Fatal("runtime release lineage accepted the wrong exact tag")
	}
}

func TestRuntimeReleaseArchiveIsDeterministicAndComplete(t *testing.T) {
	first := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer first.Close()
	second := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer second.Close()
	write := func(release *RuntimeRelease) ([]byte, RuntimeReleasePackage) {
		var output bytes.Buffer
		result, err := writeRuntimeReleaseArchive(
			context.Background(),
			release,
			[]byte("bundle"),
			[]byte("trusted root"),
			t.TempDir(),
			&output,
			runtimeReleaseAuthenticatorForTest,
			toolchainReleaseAuthenticatorForTest,
			runtimeReleaseSnapshotForTest,
			toolchainReleaseSnapshotForTest,
		)
		if err != nil {
			t.Fatal(err)
		}
		return output.Bytes(), result
	}
	firstRaw, result := write(first)
	secondRaw, _ := write(second)
	if !bytes.Equal(firstRaw, secondRaw) {
		t.Fatal("complete runtime release archive is not deterministic")
	}
	if result.Digest != runtimeReleaseDigest(firstRaw) ||
		result.SizeBytes != int64(len(firstRaw)) {
		t.Fatalf("runtime release archive result = %#v", result)
	}
	members := readRuntimePackageMembers(t, firstRaw)
	for _, name := range runtimeReleaseArchiveFiles {
		if len(members[name]) == 0 {
			t.Fatalf("runtime release archive omits %q", name)
		}
	}
	object := "objects/sha256/" +
		strings.TrimPrefix(first.valid.Digest, "sha256:")
	if !bytes.Equal(members[object], []byte("runtime")) {
		t.Fatal("runtime release archive does not retain exact runtime object")
	}
	for digest, snapshot := range first.toolchainObjects {
		name := ToolchainReleaseObjectsDirectory + "/sha256/" +
			strings.TrimPrefix(digest, "sha256:")
		if !bytes.Equal(
			members[name],
			[]byte("standard toolchain closure"),
		) || snapshot.descriptor.Digest != digest {
			t.Fatal(
				"runtime release archive does not retain exact standard-toolchain object",
			)
		}
	}
}

func TestRuntimeReleaseArchiveRejectsMemberDrift(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	var output bytes.Buffer
	if _, err := writeRuntimeReleaseArchive(
		context.Background(),
		release,
		[]byte("bundle"),
		[]byte("trusted root"),
		t.TempDir(),
		&output,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
		runtimeReleaseSnapshotForTest,
		toolchainReleaseSnapshotForTest,
	); err != nil {
		t.Fatal(err)
	}
	original := readRuntimePackageMembers(t, output.Bytes())
	names := make([]string, 0, len(original))
	for name := range original {
		names = append(names, name)
	}
	sort.Strings(names)
	tests := map[string]struct {
		names    []string
		mutate   func(*tar.Header)
		trailing []byte
	}{
		"missing": {
			names: names[1:],
		},
		"extra": {
			names: append(append([]string(nil), names...), "unexpected"),
		},
		"duplicate": {
			names: append(
				append([]string(nil), names[:1]...),
				names...,
			),
		},
		"unsafe": {
			names: append([]string{"../attestation.json"}, names[1:]...),
		},
		"symlink": {
			names: names,
			mutate: func(header *tar.Header) {
				if header.Name == RuntimeReleaseStatementFile {
					header.Typeflag = tar.TypeSymlink
					header.Linkname = "target"
				}
			},
		},
		"trailing": {
			names:    names,
			trailing: []byte("trailing"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			raw := runtimeReleaseTestTar(t, original, test.names, test.mutate)
			raw = append(raw, test.trailing...)
			if err := extractRuntimeReleaseArchive(
				bytes.NewReader(raw),
				t.TempDir(),
			); err == nil {
				t.Fatal("runtime release archive accepted member drift")
			}
		})
	}
}

func TestRuntimeReleaseArchiveChecksOuterReferenceBeforeTrust(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-release.tar")
	raw := []byte("not a trusted archive")
	if err := os.WriteFile(path, raw, 0o400); err != nil {
		t.Fatal(err)
	}
	authenticated := false
	authenticate := func(
		RuntimeReleaseLineage,
		[]byte,
		[]byte,
		[]byte,
	) (*RuntimeCatalog, error) {
		authenticated = true
		return nil, errors.New("trust must not be reached")
	}
	for name, reference := range map[string]RuntimeReleaseRef{
		"digest": {
			Release:   "v1.2.3",
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: int64(len(raw)),
		},
		"size": {
			Release:   "v1.2.3",
			Digest:    runtimeReleaseDigest(raw),
			SizeBytes: int64(len(raw) + 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			authenticated = false
			predecessor, err := openRuntimeReleaseArchive(
				context.Background(),
				path,
				reference,
				t.TempDir(),
				authenticate,
				toolchainReleaseLineageAuthenticatorForTest,
			)
			if predecessor != nil {
				predecessor.Close()
			}
			if err == nil {
				t.Fatal("runtime release archive accepted outer reference drift")
			}
			if authenticated {
				t.Fatal("runtime release archive reached embedded trust first")
			}
		})
	}
}

func TestRuntimeReleaseArchiveRejectsUnsignedStatementDrift(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	var output bytes.Buffer
	if _, err := writeRuntimeReleaseArchive(
		context.Background(),
		release,
		[]byte("bundle"),
		[]byte("trusted root"),
		t.TempDir(),
		&output,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
		runtimeReleaseSnapshotForTest,
		toolchainReleaseSnapshotForTest,
	); err != nil {
		t.Fatal(err)
	}
	baseMembers := readRuntimePackageMembers(t, output.Bytes())
	for _, statementFile := range []string{
		RuntimeReleaseStatementFile,
		ToolchainReleaseStatementFile,
	} {
		t.Run(statementFile, func(t *testing.T) {
			members := make(map[string][]byte, len(baseMembers))
			for name, raw := range baseMembers {
				members[name] = append([]byte(nil), raw...)
			}
			members[statementFile][0] ^= 0xff
			names := make([]string, 0, len(members))
			for name := range members {
				names = append(names, name)
			}
			sort.Strings(names)
			raw := runtimeReleaseTestTar(t, members, names, nil)
			path := filepath.Join(t.TempDir(), "runtime-release.tar")
			if err := os.WriteFile(path, raw, 0o400); err != nil {
				t.Fatal(err)
			}
			predecessor, err := openRuntimeReleaseArchive(
				context.Background(),
				path,
				RuntimeReleaseRef{
					Release:   release.lineage.Release,
					Digest:    runtimeReleaseDigest(raw),
					SizeBytes: int64(len(raw)),
				},
				t.TempDir(),
				func(
					_ RuntimeReleaseLineage,
					catalog,
					bundle,
					trustedRoot []byte,
				) (*RuntimeCatalog, error) {
					return runtimeReleaseAuthenticatorForTest(
						catalog,
						bundle,
						trustedRoot,
					)
				},
				toolchainReleaseLineageAuthenticatorForTest,
			)
			if predecessor != nil {
				predecessor.Close()
			}
			if err == nil {
				t.Fatal("runtime release archive accepted unsigned statement drift")
			}
		})
	}
}

func TestPrepareRuntimeReleaseRequiresExplicitLineage(t *testing.T) {
	raw := []byte("runtime")
	source := RuntimeReleaseSource{
		Runtime:          runtimeReleaseTestFile(t, raw),
		Invalid:          runtimeReleaseTestFile(t, runtimeReleaseInvalidForTest()),
		Descriptor:       runtimeReleaseDescriptor(ArchitectureX8664, raw),
		ScratchDirectory: t.TempDir(),
		UnitCgroupRoot:   "/cgroup",
		Lineage: RuntimeReleaseLineage{
			FormatVersion: 0,
			Release:       "v1.2.3",
		},
	}
	for name, mutate := range map[string]func(*RuntimeReleaseSource){
		"missing distribution": func(value *RuntimeReleaseSource) {
			value.Lineage.Predecessor = &RuntimeReleaseRef{
				Release:   "v1.2.2",
				Digest:    "sha256:" + strings.Repeat("a", 64),
				SizeBytes: 1,
			}
		},
		"unexpected distribution": func(value *RuntimeReleaseSource) {
			value.Predecessor = &RuntimeReleasePredecessor{
				Lineage: RuntimeReleaseLineage{
					FormatVersion: 0,
					Release:       "v1.2.2",
				},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			divergent := source
			mutate(&divergent)
			release, err := prepareRuntimeRelease(
				context.Background(),
				divergent,
				nil,
				nil,
				func(
					context.Context,
					string,
					string,
					*RuntimeArtifactSnapshot,
				) (RuntimeIndex, error) {
					return RuntimeIndex{}, errors.New("must not verify")
				},
				runtimeReleaseSnapshotForTest,
				toolchainReleaseSnapshotForTest,
			)
			if err == nil {
				release.Close()
				t.Fatal("runtime release accepted ambiguous lineage")
			}
		})
	}
}

func TestRuntimeWorkerPackageIsDeterministicAndUsesCapturedValidFixture(t *testing.T) {
	raw := []byte("captured runtime bytes")
	firstRelease := prepareRuntimeReleaseForTest(t, raw, nil)
	defer firstRelease.Close()
	secondRelease := prepareRuntimeReleaseForTest(t, raw, nil)
	defer secondRelease.Close()

	first := writeRuntimePackageForTest(t, firstRelease)
	second := writeRuntimePackageForTest(t, secondRelease)
	if !bytes.Equal(first, second) {
		t.Fatal("runtime worker package is not deterministic")
	}
	if err := validateRuntimeWorkerPackage(
		bytes.NewReader(first),
		ArchitectureX8664,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
	); err != nil {
		t.Fatal(err)
	}
	members := readRuntimePackageMembers(t, first)
	if !bytes.Equal(members[RuntimeReleaseValidFile], raw) {
		t.Fatal("package valid fixture differs from the captured runtime")
	}
	var names []string
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	expected := runtimeWorkerPackageNamesForTest(t, firstRelease)
	sort.Strings(expected)
	if strings.Join(names, ",") != strings.Join(expected, ",") {
		t.Fatalf("package members = %q", names)
	}
}

func TestRuntimeReleaseToolchainIterationSelectsExactArchitecture(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	catalog, err := ParseToolchainCatalog(release.toolchainCatalog)
	if err != nil {
		t.Fatal(err)
	}
	foreign := catalog.toolchains[0]
	foreign.Architecture = ArchitectureAArch64
	foreign.ManagedRuntimeDigest = toolDigestForTest("aarch64 runtime")
	foreignRaw := []byte("aarch64 standard toolchain")
	foreign.ToolchainClosure = ManagerArtifact{
		Digest:    runtimeReleaseDigest(foreignRaw),
		MediaType: ToolchainMediaType,
		SizeBytes: int64(len(foreignRaw)),
	}
	toolchains := append(catalog.toolchains, foreign)
	sort.Slice(toolchains, func(left, right int) bool {
		leftDigest, _ := StandardToolchainDigest(toolchains[left])
		rightDigest, _ := StandardToolchainDigest(toolchains[right])
		return leftDigest < rightDigest
	})
	release.toolchainCatalog, err = CanonicalToolchainCatalog(toolchains)
	if err != nil {
		t.Fatal(err)
	}
	foreignSnapshot, err := toolchainReleaseSnapshotForTest(
		context.Background(),
		t.TempDir(),
		foreign.ToolchainClosure,
		bytes.NewReader(foreignRaw),
	)
	if err != nil {
		t.Fatal(err)
	}
	release.toolchainObjects[foreign.ToolchainClosure.Digest] = foreignSnapshot

	var visited []ToolObject
	if err := release.ForEachToolchain(
		context.Background(),
		ArchitectureX8664,
		func(descriptor ToolObject, _ io.Reader) error {
			visited = append(visited, descriptor)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(visited) != 1 ||
		visited[0].Digest == foreign.ToolchainClosure.Digest {
		t.Fatalf("x86_64 toolchain iteration = %#v", visited)
	}
}

func TestRuntimeWorkerPackageRejectsMemberDrift(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	original := readRuntimePackageMembers(t, writeRuntimePackageForTest(t, release))

	tests := map[string]struct {
		names    []string
		mutate   func(*tar.Header)
		trailing []byte
	}{
		"missing": {
			names: runtimeReleasePackageFiles[:len(runtimeReleasePackageFiles)-1],
		},
		"extra": {
			names: append(
				append([]string(nil), runtimeReleasePackageFiles...),
				"extra",
			),
		},
		"duplicate": {
			names: []string{
				RuntimeReleaseCatalogFile,
				RuntimeReleaseCatalogFile,
				RuntimeReleaseTrustedRootFile,
				RuntimeReleaseCorpusFile,
				RuntimeReleaseInvalidFile,
				RuntimeReleaseValidFile,
			},
		},
		"unsafe": {
			names: []string{
				"../catalog.json",
				RuntimeReleaseBundleFile,
				RuntimeReleaseTrustedRootFile,
				RuntimeReleaseCorpusFile,
				RuntimeReleaseInvalidFile,
				RuntimeReleaseValidFile,
			},
		},
		"symlink": {
			names: runtimeReleasePackageFiles,
			mutate: func(header *tar.Header) {
				if header.Name == RuntimeReleaseCatalogFile {
					header.Typeflag = tar.TypeSymlink
					header.Linkname = "target"
				}
			},
		},
		"trailing": {
			names:    runtimeReleasePackageFiles,
			trailing: []byte("trailing"),
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			raw := runtimeReleaseTestTar(t, original, test.names, test.mutate)
			raw = append(raw, test.trailing...)
			if err := validateRuntimeWorkerPackage(
				bytes.NewReader(raw),
				ArchitectureX8664,
				runtimeReleaseAuthenticatorForTest,
				toolchainReleaseAuthenticatorForTest,
			); err == nil {
				t.Fatal("runtime package accepted member drift")
			}
		})
	}
}

func TestRuntimeWorkerPackageRejectsDigestAndCatalogMembershipDrift(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	original := readRuntimePackageMembers(t, writeRuntimePackageForTest(t, release))

	t.Run("valid digest", func(t *testing.T) {
		members := cloneRuntimeReleaseMembers(original)
		members[RuntimeReleaseValidFile][0] ^= 0xff
		raw := runtimeReleaseTestTar(
			t,
			members,
			runtimeReleasePackageFiles,
			nil,
		)
		if err := validateRuntimeWorkerPackage(
			bytes.NewReader(raw),
			ArchitectureX8664,
			runtimeReleaseAuthenticatorForTest,
			toolchainReleaseAuthenticatorForTest,
		); err == nil {
			t.Fatal("runtime package accepted valid fixture digest drift")
		}
	})

	t.Run("invalid digest", func(t *testing.T) {
		members := cloneRuntimeReleaseMembers(original)
		members[RuntimeReleaseInvalidFile][0] = 1
		raw := runtimeReleaseTestTar(
			t,
			members,
			runtimeReleasePackageFiles,
			nil,
		)
		if err := validateRuntimeWorkerPackage(
			bytes.NewReader(raw),
			ArchitectureX8664,
			runtimeReleaseAuthenticatorForTest,
			toolchainReleaseAuthenticatorForTest,
		); err == nil {
			t.Fatal("runtime package accepted invalid fixture digest drift")
		}
	})

	t.Run("standard toolchain digest", func(t *testing.T) {
		members := cloneRuntimeReleaseMembers(original)
		var objectName string
		for name := range members {
			if strings.HasPrefix(
				name,
				ToolchainReleaseObjectsDirectory+"/sha256/",
			) {
				objectName = name
				break
			}
		}
		if objectName == "" {
			t.Fatal("worker package has no standard-toolchain object")
		}
		members[objectName][0] ^= 0xff
		names := runtimeWorkerPackageNamesForTest(t, release)
		raw := runtimeReleaseTestTar(t, members, names, nil)
		if err := validateRuntimeWorkerPackage(
			bytes.NewReader(raw),
			ArchitectureX8664,
			runtimeReleaseAuthenticatorForTest,
			toolchainReleaseAuthenticatorForTest,
		); err == nil {
			t.Fatal("runtime package accepted standard-toolchain digest drift")
		}
	})

	t.Run("catalog membership", func(t *testing.T) {
		members := cloneRuntimeReleaseMembers(original)
		catalog, err := ParseRuntimeCatalog(members[RuntimeReleaseCatalogFile])
		if err != nil {
			t.Fatal(err)
		}
		document, err := parseRuntimeVerifierCorpusManifest(
			members[RuntimeReleaseCorpusFile],
			authenticatedRuntimeCatalogForTest(t, catalog.runtimes),
			ArchitectureX8664,
		)
		if err != nil {
			t.Fatal(err)
		}
		document.Valid.Descriptor.Digest = "sha256:" + strings.Repeat("f", 64)
		members[RuntimeReleaseCorpusFile], err =
			canonicalRuntimeVerifierCorpusManifest(document)
		if err != nil {
			t.Fatal(err)
		}
		raw := runtimeReleaseTestTar(
			t,
			members,
			runtimeReleasePackageFiles,
			nil,
		)
		if err := validateRuntimeWorkerPackage(
			bytes.NewReader(raw),
			ArchitectureX8664,
			runtimeReleaseAuthenticatorForTest,
			toolchainReleaseAuthenticatorForTest,
		); err == nil {
			t.Fatal("runtime package accepted a non-member valid descriptor")
		}
	})
}

func TestRuntimeWorkerPackageRejectsToolchainTrustedRootDivergence(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	members := readRuntimePackageMembers(t, writeRuntimePackageForTest(t, release))
	members[ToolchainReleaseTrustedRootFile] = []byte("another trusted root")
	raw := runtimeReleaseTestTar(
		t,
		members,
		runtimeReleasePackageFiles,
		nil,
	)
	if err := validateRuntimeWorkerPackage(
		bytes.NewReader(raw),
		ArchitectureX8664,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
	); err == nil {
		t.Fatal("runtime package accepted divergent standard-toolchain trusted roots")
	}
}

func TestRuntimeCatalogSuccessorRejectsRemovalAndMutation(t *testing.T) {
	first := runtimeReleaseDescriptor(ArchitectureX8664, []byte("first"))
	second := runtimeReleaseDescriptor(ArchitectureX8664, []byte("second"))
	runtimes := []RuntimeDescriptor{first, second}
	sort.Slice(runtimes, func(left, right int) bool {
		return runtimes[left].Digest < runtimes[right].Digest
	})
	predecessor := authenticatedRuntimeCatalogForTest(t, runtimes)

	if err := validateRuntimeCatalogSuccessor(predecessor, runtimes[:1]); err == nil {
		t.Fatal("runtime catalog successor accepted predecessor removal")
	}
	mutated := append([]RuntimeDescriptor(nil), runtimes...)
	mutated[0].SizeBytes++
	if err := validateRuntimeCatalogSuccessor(predecessor, mutated); err == nil {
		t.Fatal("runtime catalog successor accepted predecessor mutation")
	}
	if err := validateRuntimeCatalogSuccessor(predecessor, runtimes); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRuntimeReleaseRetainsExactPredecessorObjects(t *testing.T) {
	priorRaw := []byte("predecessor runtime")
	priorDescriptor := runtimeReleaseDescriptor(ArchitectureX8664, priorRaw)
	predecessorCatalog := authenticatedRuntimeCatalogForTest(
		t,
		[]RuntimeDescriptor{priorDescriptor},
	)
	priorFile := runtimeReleaseTestFile(t, priorRaw)
	predecessor := &RuntimeReleasePredecessor{
		Runtimes: map[string]*os.File{priorDescriptor.Digest: priorFile},
	}
	release := prepareRuntimeReleaseForTestWithCatalog(
		t,
		[]byte("successor runtime"),
		predecessor,
		predecessorCatalog,
	)
	defer release.Close()

	var objects [][]byte
	if err := release.ForEachRuntime(
		context.Background(),
		func(_ RuntimeDescriptor, source io.Reader) error {
			raw, err := io.ReadAll(source)
			objects = append(objects, raw)
			return err
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("retained object count = %d, want 2", len(objects))
	}
	found := false
	for _, raw := range objects {
		found = found || bytes.Equal(raw, priorRaw)
	}
	if !found {
		t.Fatal("predecessor runtime bytes were not retained")
	}
	if len(release.toolchainObjects) != 1 {
		t.Fatalf(
			"deduplicated standard-toolchain object count = %d, want 1",
			len(release.toolchainObjects),
		)
	}
	for _, object := range release.toolchainObjects {
		reader, err := object.content.uploadReader(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, []byte("standard toolchain closure")) {
			t.Fatal("predecessor standard-toolchain bytes were not retained")
		}
	}
}

func TestPrepareRuntimeReleaseRejectsPredecessorObjectDrift(t *testing.T) {
	priorRaw := []byte("predecessor runtime")
	descriptor := runtimeReleaseDescriptor(ArchitectureX8664, priorRaw)
	catalog := authenticatedRuntimeCatalogForTest(t, []RuntimeDescriptor{descriptor})
	drifted := runtimeReleaseTestFile(t, []byte("predecessor runtimf"))
	predecessor := &RuntimeReleasePredecessor{
		Runtimes: map[string]*os.File{descriptor.Digest: drifted},
	}
	if release, err := prepareRuntimeReleaseWithCatalogForTest(
		t,
		[]byte("successor"),
		predecessor,
		catalog,
	); err == nil {
		release.Close()
		t.Fatal("runtime release accepted predecessor object drift")
	}
}

func TestVerifyRuntimeWorkerPackageUsesProductionOutcomeBeforeMaterializing(t *testing.T) {
	release := prepareRuntimeReleaseForTest(t, []byte("runtime"), nil)
	defer release.Close()
	raw := writeRuntimePackageForTest(t, release)
	source := runtimeReleaseTestFile(t, raw)
	output := filepath.Join(t.TempDir(), "release")
	snapshotOutput := filepath.Join(t.TempDir(), "runtime-release-x86_64.tar")
	var leases []string
	verify := func(
		_ context.Context,
		_ string,
		lease string,
		_ *RuntimeArtifactSnapshot,
	) (RuntimeIndex, error) {
		leases = append(leases, lease)
		switch lease {
		case "package-valid":
			return release.index, nil
		case "package-invalid":
			return RuntimeIndex{}, &verifierInvalidError{diagnostic: "runtime is invalid"}
		default:
			return RuntimeIndex{}, errors.New("unexpected lease")
		}
	}
	result, err := verifyRuntimeWorkerPackage(
		context.Background(),
		source,
		ArchitectureX8664,
		"/cgroup",
		t.TempDir(),
		output,
		snapshotOutput,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
		verify,
		runtimeReleaseSnapshotForTest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.SizeBytes != int64(len(raw)) || result.Digest != runtimeReleaseDigest(raw) {
		t.Fatalf("package result = %#v", result)
	}
	if strings.Join(leases, ",") != "package-valid,package-invalid" {
		t.Fatalf("verifier leases = %q", leases)
	}
	snapshotRaw, err := os.ReadFile(snapshotOutput)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(snapshotRaw, raw) {
		t.Fatal("verified runtime package snapshot differs from captured input")
	}
	snapshotStat, err := os.Stat(snapshotOutput)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotStat.Mode().Perm() != 0o444 {
		t.Fatalf("verified runtime package snapshot mode = %v", snapshotStat.Mode())
	}
	for _, name := range runtimeWorkerPackageNamesForTest(t, release) {
		stat, err := os.Stat(filepath.Join(output, name))
		if err != nil {
			t.Fatal(err)
		}
		if !stat.Mode().IsRegular() || stat.Mode().Perm() != 0o444 {
			t.Fatalf("materialized %s mode = %v", name, stat.Mode())
		}
	}

	failedOutput := filepath.Join(t.TempDir(), "release")
	failedSource := runtimeReleaseTestFile(t, raw)
	_, err = verifyRuntimeWorkerPackage(
		context.Background(),
		failedSource,
		ArchitectureX8664,
		"/cgroup",
		t.TempDir(),
		failedOutput,
		"",
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
		func(
			context.Context,
			string,
			string,
			*RuntimeArtifactSnapshot,
		) (RuntimeIndex, error) {
			return RuntimeIndex{}, errors.New("verifier infrastructure failure")
		},
		runtimeReleaseSnapshotForTest,
	)
	if err == nil {
		t.Fatal("runtime package accepted verifier infrastructure failure")
	}
	if _, err := os.Stat(failedOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed runtime package verification left output residue")
	}
}

func TestPinnedRuntimeCatalogAuthenticatorRejectsEmbeddedRootDrift(t *testing.T) {
	authenticate := pinnedRuntimeCatalogAuthenticator(
		RuntimeReleaseLineage{FormatVersion: 0, Release: "v1.2.3"},
		[]byte("pinned trusted root"),
	)
	if _, err := authenticate(
		nil,
		nil,
		[]byte("embedded trusted root"),
	); err == nil || !strings.Contains(err.Error(), "does not exact-match") {
		t.Fatalf("pinned trusted-root mismatch error = %v", err)
	}
}

func TestMaterializeRuntimeWorkerSnapshotIsCreateOnly(t *testing.T) {
	destination := filepath.Join(t.TempDir(), "runtime-release-x86_64.tar")
	if err := os.WriteFile(destination, []byte("existing"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := materializeRuntimeWorkerSnapshot(
		context.Background(),
		strings.NewReader("replacement"),
		int64(len("replacement")),
		destination,
	); err == nil {
		t.Fatal("runtime package snapshot replaced an existing destination")
	}
	raw, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "existing" {
		t.Fatalf("runtime package snapshot bytes = %q", raw)
	}
}

func prepareRuntimeReleaseForTest(
	t *testing.T,
	raw []byte,
	predecessor *RuntimeReleasePredecessor,
) *RuntimeRelease {
	t.Helper()
	return prepareRuntimeReleaseForTestWithCatalog(t, raw, predecessor, nil)
}

func prepareRuntimeReleaseForTestWithCatalog(
	t *testing.T,
	raw []byte,
	predecessor *RuntimeReleasePredecessor,
	catalog *RuntimeCatalog,
) *RuntimeRelease {
	t.Helper()
	release, err := prepareRuntimeReleaseWithCatalogForTest(
		t,
		raw,
		predecessor,
		catalog,
	)
	if err != nil {
		t.Fatal(err)
	}
	return release
}

func prepareRuntimeReleaseWithCatalogForTest(
	t *testing.T,
	raw []byte,
	predecessor *RuntimeReleasePredecessor,
	catalog *RuntimeCatalog,
) (*RuntimeRelease, error) {
	t.Helper()
	descriptor := runtimeReleaseDescriptor(ArchitectureX8664, raw)
	source := runtimeReleaseTestFile(t, raw)
	invalid := runtimeReleaseTestFile(t, runtimeReleaseInvalidForTest())
	index := RuntimeIndex{
		Architecture:      ArchitectureX8664,
		FormatVersion:     RuntimeIndexFormatVersion,
		RuntimeAPIVersion: RuntimeAPIVersion,
	}
	lineage := RuntimeReleaseLineage{
		FormatVersion: 0,
		Release:       "v1.2.3",
	}
	if predecessor != nil {
		predecessor.Lineage = RuntimeReleaseLineage{
			FormatVersion: 0,
			Release:       "v1.2.2",
		}
		lineage.Predecessor = &RuntimeReleaseRef{
			Release:   "v1.2.2",
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 1,
		}
	}
	toolchain := testToolchain(t)
	toolchainRaw := []byte("standard toolchain closure")
	toolchain.ToolchainClosure = ManagerArtifact{
		Digest:    runtimeReleaseDigest(toolchainRaw),
		MediaType: ToolchainMediaType,
		SizeBytes: int64(len(toolchainRaw)),
	}
	toolchain.Architecture = descriptor.Architecture
	toolchain.ManagedRuntimeDigest = descriptor.Digest
	toolchainSource, err := runtimeReleaseToolchainSourceForTest(
		t,
		toolchain,
		toolchainRaw,
	)
	if err != nil {
		return nil, err
	}
	if predecessor != nil && len(predecessor.ToolchainCatalog) == 0 {
		predecessor.ToolchainCatalog, err = CanonicalToolchainCatalog(
			[]Toolchain{toolchain},
		)
		if err != nil {
			return nil, err
		}
		predecessor.ToolchainBundle = []byte("toolchain bundle")
		predecessor.ToolchainTrustedRoot = []byte("trusted root")
		predecessor.Toolchains = map[string]*os.File{
			toolchain.ToolchainClosure.Digest: runtimeReleaseTestFile(
				t,
				toolchainRaw,
			),
		}
	}
	var predecessorToolchains *ToolchainCatalog
	if predecessor != nil {
		predecessorToolchains, err = ParseToolchainCatalog(
			predecessor.ToolchainCatalog,
		)
		if err != nil {
			return nil, err
		}
		predecessorToolchains.authenticated = true
	}
	release, err := prepareRuntimeRelease(
		context.Background(),
		RuntimeReleaseSource{
			Runtime:                  source,
			Invalid:                  invalid,
			Descriptor:               descriptor,
			ScratchDirectory:         t.TempDir(),
			UnitCgroupRoot:           "/cgroup",
			Lineage:                  lineage,
			Predecessor:              predecessor,
			ToolchainSourceDirectory: toolchainSource,
		},
		catalog,
		predecessorToolchains,
		func(
			_ context.Context,
			_ string,
			lease string,
			_ *RuntimeArtifactSnapshot,
		) (RuntimeIndex, error) {
			if lease == "release-valid" {
				return index, nil
			}
			if lease == "release-invalid" {
				return RuntimeIndex{}, &verifierInvalidError{
					diagnostic: "runtime is invalid",
				}
			}
			return RuntimeIndex{}, errors.New("unexpected verifier lease")
		},
		runtimeReleaseSnapshotForTest,
		toolchainReleaseSnapshotForTest,
	)
	if err != nil {
		return nil, err
	}
	release.toolchainBundle = []byte("toolchain bundle")
	release.toolchainTrustedRoot = []byte("trusted root")
	return release, nil
}

func runtimeReleaseToolchainSourceForTest(
	t *testing.T,
	toolchain Toolchain,
	raw []byte,
) (string, error) {
	t.Helper()
	directory := t.TempDir()
	document, err := CanonicalToolchain(toolchain)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(
		filepath.Join(directory, ToolchainSourceFile),
		document,
		0o444,
	); err != nil {
		return "", err
	}
	objectDirectory := filepath.Join(directory, "objects", "sha256")
	if err := os.MkdirAll(objectDirectory, 0o755); err != nil {
		return "", err
	}
	name := strings.TrimPrefix(toolchain.ToolchainClosure.Digest, "sha256:")
	if err := os.WriteFile(
		filepath.Join(objectDirectory, name),
		raw,
		0o444,
	); err != nil {
		return "", err
	}
	return directory, nil
}

func runtimeReleaseSnapshotForTest(
	ctx context.Context,
	_ string,
	descriptor RuntimeDescriptor,
	source *os.File,
) (*RuntimeArtifactSnapshot, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(raw)) != descriptor.SizeBytes ||
		runtimeReleaseDigest(raw) != descriptor.Digest {
		return nil, errors.New("runtime release test snapshot descriptor drift")
	}
	file, err := os.CreateTemp("", "helmr-runtime-release-test-*")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	verifier, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	upload, err := os.Open(path)
	if err != nil {
		verifier.Close()
		return nil, err
	}
	snapshotDescriptor := artifactSnapshotDescriptor{
		Digest:    descriptor.Digest,
		MediaType: descriptor.MediaType,
		SizeBytes: descriptor.SizeBytes,
	}
	return &RuntimeArtifactSnapshot{
		descriptor: descriptor,
		content: &artifactSnapshot{
			descriptor: snapshotDescriptor,
			verifier:   verifier,
			upload:     upload,
		},
	}, nil
}

func toolchainReleaseSnapshotForTest(
	ctx context.Context,
	_ string,
	descriptor ManagerArtifact,
	source io.Reader,
) (*toolchainSnapshot, error) {
	raw, err := io.ReadAll(source)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(raw)) != descriptor.SizeBytes ||
		runtimeReleaseDigest(raw) != descriptor.Digest {
		return nil, errors.New(
			"standard-toolchain test snapshot descriptor drift",
		)
	}
	path := filepath.Join(os.TempDir(), strings.TrimPrefix(
		descriptor.Digest,
		"sha256:",
	))
	file, err := os.CreateTemp(filepath.Dir(path), ".helmr-toolchain-test-*")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(raw); err != nil {
		file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	verifier, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	upload, err := os.Open(name)
	if err != nil {
		verifier.Close()
		return nil, err
	}
	return &toolchainSnapshot{
		descriptor: descriptor,
		content: &artifactSnapshot{
			descriptor: artifactSnapshotDescriptor{
				Digest:    descriptor.Digest,
				MediaType: descriptor.MediaType,
				SizeBytes: descriptor.SizeBytes,
			},
			verifier: verifier,
			upload:   upload,
		},
	}, nil
}

func runtimeReleaseTestFile(t *testing.T, raw []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		file.Close()
	})
	return file
}

func runtimeReleaseInvalidForTest() []byte {
	raw := make([]byte, runtimeVerifierCorpusInvalidBytes)
	copy(raw, "hsqs: deterministic topology-invalid fixture")
	return raw
}

func writeRuntimePackageForTest(t *testing.T, release *RuntimeRelease) []byte {
	t.Helper()
	var output bytes.Buffer
	if _, err := writeRuntimeWorkerPackage(
		context.Background(),
		release,
		[]byte("bundle"),
		[]byte("trusted root"),
		t.TempDir(),
		&output,
		runtimeReleaseAuthenticatorForTest,
		toolchainReleaseAuthenticatorForTest,
	); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func runtimeWorkerPackageNamesForTest(
	t *testing.T,
	release *RuntimeRelease,
) []string {
	t.Helper()
	names := append([]string(nil), runtimeReleasePackageFiles...)
	catalog, err := ParseToolchainCatalog(release.toolchainCatalog)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := toolchainClosureObjects(
		catalog,
		release.architecture,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range objects {
		names = append(
			names,
			ToolchainReleaseObjectsDirectory+"/sha256/"+
				strings.TrimPrefix(object.Digest, "sha256:"),
		)
	}
	return names
}

func toolchainReleaseAuthenticatorForTest(
	catalogBytes,
	bundle,
	trustedRoot []byte,
) (*ToolchainCatalog, error) {
	if string(bundle) != "toolchain bundle" ||
		string(trustedRoot) != "trusted root" {
		return nil, errors.New("unexpected standard-toolchain release trust inputs")
	}
	catalog, err := ParseToolchainCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func toolchainReleaseLineageAuthenticatorForTest(
	_ RuntimeReleaseLineage,
	catalog,
	bundle,
	trustedRoot []byte,
) (*ToolchainCatalog, error) {
	return toolchainReleaseAuthenticatorForTest(
		catalog,
		bundle,
		trustedRoot,
	)
}

func runtimeReleaseAuthenticatorForTest(
	catalogBytes,
	bundle,
	trustedRoot []byte,
) (*RuntimeCatalog, error) {
	if string(bundle) != "bundle" || string(trustedRoot) != "trusted root" {
		return nil, errors.New("unexpected runtime release trust inputs")
	}
	catalog, err := ParseRuntimeCatalog(catalogBytes)
	if err != nil {
		return nil, err
	}
	catalog.authenticated = true
	return catalog, nil
}

func readRuntimePackageMembers(t *testing.T, raw []byte) map[string][]byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(raw))
	members := make(map[string][]byte)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return members
		}
		if err != nil {
			t.Fatal(err)
		}
		value, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		members[header.Name] = value
	}
}

func runtimeReleaseTestTar(
	t *testing.T,
	members map[string][]byte,
	names []string,
	mutate func(*tar.Header),
) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := tar.NewWriter(&output)
	for _, name := range names {
		raw := members[name]
		if raw == nil {
			raw = []byte("extra")
		}
		header := &tar.Header{
			Name:     name,
			Mode:     runtimeReleasePackageFileMode,
			Size:     int64(len(raw)),
			ModTime:  unixEpochForTest(),
			Typeflag: tar.TypeReg,
			Format:   tar.FormatUSTAR,
		}
		if mutate != nil {
			mutate(header)
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func cloneRuntimeReleaseMembers(source map[string][]byte) map[string][]byte {
	clone := make(map[string][]byte, len(source))
	for name, raw := range source {
		clone[name] = append([]byte(nil), raw...)
	}
	return clone
}

func runtimeReleaseDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func unixEpochForTest() time.Time {
	return time.Unix(0, 0).UTC()
}
