package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestRuntimeVerifierCorpusPathsAreFixed(t *testing.T) {
	if runtimeVerifierCorpusManifestPath != "/usr/lib/helmr/runtime-release/verifier-corpus.json" {
		t.Fatalf("runtime verifier corpus manifest path = %q", runtimeVerifierCorpusManifestPath)
	}
	if runtimeVerifierCorpusValidPath != "/usr/lib/helmr/runtime-release/verifier-valid.squashfs" {
		t.Fatalf("runtime verifier valid fixture path = %q", runtimeVerifierCorpusValidPath)
	}
	if runtimeVerifierCorpusInvalidPath != "/usr/lib/helmr/runtime-release/verifier-invalid.squashfs" {
		t.Fatalf("runtime verifier invalid fixture path = %q", runtimeVerifierCorpusInvalidPath)
	}
}

func TestOpenRuntimeVerifierCorpusValidatesClosedRelease(t *testing.T) {
	paths, catalog, architecture, expected := writeRuntimeVerifierCorpus(t)
	corpus, err := openRuntimeVerifierCorpus(
		paths,
		catalog,
		architecture,
		uint32(os.Getuid()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	if corpus.document != expected {
		t.Fatalf("runtime verifier corpus = %#v, want %#v", corpus.document, expected)
	}
}

func TestRuntimeVerifierCorpusRunsProductionContractOrder(t *testing.T) {
	paths, catalog, architecture, expected := writeRuntimeVerifierCorpus(t)
	corpus, err := openRuntimeVerifierCorpus(
		paths,
		catalog,
		architecture,
		uint32(os.Getuid()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()

	var snapshots []RuntimeDescriptor
	snapshotter := func(
		_ context.Context,
		_ string,
		descriptor RuntimeDescriptor,
		source io.Reader,
	) (*RuntimeArtifactSnapshot, error) {
		raw, err := io.ReadAll(source)
		if err != nil {
			return nil, err
		}
		if int64(len(raw)) != descriptor.SizeBytes {
			return nil, errors.New("fixture size did not match descriptor")
		}
		sum := sha256.Sum256(raw)
		if "sha256:"+hex.EncodeToString(sum[:]) != descriptor.Digest {
			return nil, errors.New("fixture digest did not match descriptor")
		}
		snapshots = append(snapshots, descriptor)
		return &RuntimeArtifactSnapshot{}, nil
	}
	var leases []string
	verifier := func(
		_ context.Context,
		_ string,
		lease string,
		_ *RuntimeArtifactSnapshot,
	) (RuntimeIndex, error) {
		leases = append(leases, lease)
		switch lease {
		case "corpus-valid":
			return expected.Valid.ExpectedIndex, nil
		case "corpus-invalid":
			return RuntimeIndex{}, &verifierInvalidError{diagnostic: "runtime is invalid"}
		default:
			return RuntimeIndex{}, errors.New("unexpected lease")
		}
	}
	if err := verifyRuntimeVerifierCorpus(
		context.Background(),
		corpus,
		"/sys/fs/cgroup/worker",
		t.TempDir(),
		snapshotter,
		verifier,
	); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 ||
		snapshots[0] != expected.Valid.Descriptor ||
		snapshots[1] != expected.Invalid.Descriptor {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if strings.Join(leases, ",") != "corpus-valid,corpus-invalid" {
		t.Fatalf("verifier leases = %q", leases)
	}
}

func TestRuntimeVerifierCorpusRequiresExactVerifierOutcomes(t *testing.T) {
	tests := map[string]runtimeCorpusVerifier{
		"valid mismatch": func(
			context.Context,
			string,
			string,
			*RuntimeArtifactSnapshot,
		) (RuntimeIndex, error) {
			return RuntimeIndex{
				Architecture:      ArchitectureAArch64,
				FormatVersion:     RuntimeIndexFormatVersion,
				RuntimeAPIVersion: RuntimeAPIVersion,
			}, nil
		},
		"invalid accepted": func(
			_ context.Context,
			_ string,
			lease string,
			_ *RuntimeArtifactSnapshot,
		) (RuntimeIndex, error) {
			if lease == "corpus-valid" {
				return RuntimeIndex{
					Architecture:      ArchitectureX8664,
					FormatVersion:     RuntimeIndexFormatVersion,
					RuntimeAPIVersion: RuntimeAPIVersion,
				}, nil
			}
			return RuntimeIndex{}, nil
		},
		"invalid infrastructure failure": func(
			_ context.Context,
			_ string,
			lease string,
			_ *RuntimeArtifactSnapshot,
		) (RuntimeIndex, error) {
			if lease == "corpus-valid" {
				return RuntimeIndex{
					Architecture:      ArchitectureX8664,
					FormatVersion:     RuntimeIndexFormatVersion,
					RuntimeAPIVersion: RuntimeAPIVersion,
				}, nil
			}
			return RuntimeIndex{}, errors.New("verifier timeout")
		},
	}
	for name, verifier := range tests {
		t.Run(name, func(t *testing.T) {
			paths, catalog, architecture, _ := writeRuntimeVerifierCorpus(t)
			corpus, err := openRuntimeVerifierCorpus(
				paths,
				catalog,
				architecture,
				uint32(os.Getuid()),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer corpus.Close()
			snapshotter := func(
				context.Context,
				string,
				RuntimeDescriptor,
				io.Reader,
			) (*RuntimeArtifactSnapshot, error) {
				return &RuntimeArtifactSnapshot{}, nil
			}
			if err := verifyRuntimeVerifierCorpus(
				context.Background(),
				corpus,
				"/sys/fs/cgroup/worker",
				t.TempDir(),
				snapshotter,
				verifier,
			); err == nil {
				t.Fatal("runtime verifier corpus accepted a divergent outcome")
			}
		})
	}
}

func TestRuntimeVerifierCorpusRejectsManifestDrift(t *testing.T) {
	_, catalog, architecture, document := writeRuntimeVerifierCorpus(t)
	canonical, err := canonicalRuntimeVerifierCorpusManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"missing version": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0,`,
			"",
			1,
		)),
		"unknown": append(
			append([]byte(nil), canonical[:len(canonical)-1]...),
			[]byte(`,"unknown":true}`)...,
		),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRuntimeVerifierCorpusManifest(
				raw,
				catalog,
				architecture,
			); err == nil {
				t.Fatal("runtime verifier corpus accepted manifest drift")
			}
		})
	}
}

func TestRuntimeVerifierCorpusRejectsDescriptorDrift(t *testing.T) {
	_, catalog, architecture, document := writeRuntimeVerifierCorpus(t)
	tests := map[string]func(*runtimeVerifierCorpusManifest){
		"valid architecture": func(value *runtimeVerifierCorpusManifest) {
			value.Valid.Descriptor.Architecture = ArchitectureAArch64
		},
		"index architecture": func(value *runtimeVerifierCorpusManifest) {
			value.Valid.ExpectedIndex.Architecture = ArchitectureAArch64
		},
		"valid catalog member": func(value *runtimeVerifierCorpusManifest) {
			value.Valid.Descriptor.SizeBytes++
		},
		"invalid size": func(value *runtimeVerifierCorpusManifest) {
			value.Invalid.Descriptor.SizeBytes++
		},
		"invalid catalog member": func(value *runtimeVerifierCorpusManifest) {
			value.Invalid.Descriptor = value.Valid.Descriptor
			value.Invalid.Descriptor.SizeBytes = runtimeVerifierCorpusInvalidBytes
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			divergent := document
			mutate(&divergent)
			raw, err := canonicalRuntimeVerifierCorpusManifest(divergent)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseRuntimeVerifierCorpusManifest(
				raw,
				catalog,
				architecture,
			); err == nil {
				t.Fatal("runtime verifier corpus accepted descriptor drift")
			}
		})
	}
}

func TestOpenRuntimeCorpusFileRejectsInsecureFiles(t *testing.T) {
	directory := t.TempDir()
	owner := uint32(os.Getuid())
	regular := filepath.Join(directory, "regular")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("wrong owner", func(t *testing.T) {
		if _, err := openReleaseFile(regular, "fixture", 1, owner+1); err == nil {
			t.Fatal("runtime corpus accepted the wrong owner")
		}
	})
	t.Run("writable by other", func(t *testing.T) {
		path := filepath.Join(directory, "writable")
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o622); err != nil {
			t.Fatal(err)
		}
		if _, err := openReleaseFile(path, "fixture", 1, owner); err == nil {
			t.Fatal("runtime corpus accepted an insecure mode")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		path := filepath.Join(directory, "link")
		if err := os.Symlink(regular, path); err != nil {
			t.Fatal(err)
		}
		if _, err := openReleaseFile(path, "fixture", 1, owner); err == nil {
			t.Fatal("runtime corpus accepted a symlink")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if _, err := openReleaseFile(directory, "fixture", 1, owner); err == nil {
			t.Fatal("runtime corpus accepted a directory")
		}
	})
	t.Run("wrong exact size", func(t *testing.T) {
		if _, err := openReleaseFileExact(regular, "fixture", 2, owner); err == nil {
			t.Fatal("runtime corpus accepted the wrong exact size")
		}
	})
}

func writeRuntimeVerifierCorpus(
	t *testing.T,
) (
	runtimeVerifierCorpusPaths,
	*RuntimeCatalog,
	RuntimeArchitecture,
	runtimeVerifierCorpusManifest,
) {
	t.Helper()
	directory := t.TempDir()
	validRaw := []byte("valid managed runtime fixture")
	invalidRaw := make([]byte, runtimeVerifierCorpusInvalidBytes)
	valid := runtimeCorpusTestDescriptor(validRaw)
	invalid := runtimeCorpusTestDescriptor(invalidRaw)
	document := runtimeVerifierCorpusManifest{
		FormatVersion: RuntimeVerifierCorpusFormatVersion,
		Valid: runtimeVerifierCorpusValid{
			Descriptor: valid,
			ExpectedIndex: RuntimeIndex{
				Architecture:      ArchitectureX8664,
				FormatVersion:     RuntimeIndexFormatVersion,
				RuntimeAPIVersion: RuntimeAPIVersion,
			},
		},
		Invalid: runtimeVerifierCorpusInvalid{Descriptor: invalid},
	}
	raw, err := canonicalRuntimeVerifierCorpusManifest(document)
	if err != nil {
		t.Fatal(err)
	}
	paths := runtimeVerifierCorpusPaths{
		manifest: filepath.Join(directory, "verifier-corpus.json"),
		valid:    filepath.Join(directory, "verifier-valid.squashfs"),
		invalid:  filepath.Join(directory, "verifier-invalid.squashfs"),
	}
	for path, content := range map[string][]byte{
		paths.manifest: raw,
		paths.valid:    validRaw,
		paths.invalid:  invalidRaw,
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtimes := []RuntimeDescriptor{valid}
	sort.Slice(runtimes, func(first, second int) bool {
		return runtimes[first].Digest < runtimes[second].Digest
	})
	catalog := authenticatedRuntimeCatalogForTest(t, runtimes)
	return paths, catalog, ArchitectureX8664, document
}

func runtimeCorpusTestDescriptor(raw []byte) RuntimeDescriptor {
	sum := sha256.Sum256(raw)
	return RuntimeDescriptor{
		Architecture:      ArchitectureX8664,
		Digest:            "sha256:" + hex.EncodeToString(sum[:]),
		FormatVersion:     RuntimeDescriptorFormatVersion,
		MediaType:         RuntimeArtifactMediaType,
		RuntimeAPIVersion: RuntimeAPIVersion,
		SizeBytes:         int64(len(raw)),
	}
}

func TestRuntimeVerifierCorpusManifestLimit(t *testing.T) {
	if maxRuntimeVerifierCorpusManifestBytes != 16<<10 {
		t.Fatalf("manifest limit = %d", maxRuntimeVerifierCorpusManifestBytes)
	}
	if runtimeVerifierCorpusInvalidBytes != 4096 {
		t.Fatalf("invalid fixture size = %d", runtimeVerifierCorpusInvalidBytes)
	}
	if runtimeVerifierCorpusInvalidBytes+maxRuntimeVerifierCorpusManifestBytes >
		runtimeVerifierCorpusScratchOverhead {
		t.Fatal("fixed corpus metadata exceeds the scratch overhead")
	}
	if _, err := parseRuntimeVerifierCorpusManifest(
		bytes.Repeat([]byte("x"), maxRuntimeVerifierCorpusManifestBytes+1),
		nil,
		ArchitectureX8664,
	); err == nil {
		t.Fatal("runtime verifier corpus accepted an oversized manifest")
	}
}
