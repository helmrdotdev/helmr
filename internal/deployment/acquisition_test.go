package deployment

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestVerifyNodeChecksumsIgnoresGPGVOutputRepresentation(t *testing.T) {
	const signer = "86C8D74642E67846F8E120284DAA80D1E737BC9F"
	gpgv := filepath.Join(t.TempDir(), "gpgv")
	script := `#!/bin/sh
set -eu
output=
while [ "$#" -gt 0 ]; do
  case "$1" in
    --status-fd|--keyring)
      shift 2
      ;;
    --output)
      output="$2"
      shift 2
      ;;
    *)
      shift
      ;;
  esac
done
test -n "$output"
printf 'version-dependent gpgv cleartext\n\n' >"$output"
printf '[GNUPG:] VALIDSIG ` + signer + ` 0 0 0 0 0 0 0 0 0 0\n'
`
	if err := os.WriteFile(gpgv, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	signedRaw, err := os.ReadFile("testdata/node-shasums-v24.12.0.asc")
	if err != nil {
		t.Fatal(err)
	}
	signedPath := filepath.Join(t.TempDir(), "SHASUMS256.txt.asc")
	if err := os.WriteFile(signedPath, signedRaw, 0400); err != nil {
		t.Fatal(err)
	}
	signed, err := os.Open(signedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer signed.Close()
	acquirer := PlatformAcquirer{GPGV: gpgv}
	plain, actualSigner, err := acquirer.verifyNodeChecksums(
		context.Background(),
		t.TempDir(),
		signed,
		NodePolicy{
			ReleaseKeyFingerprints: []string{signer},
			ReleaseKeyring:         base64.StdEncoding.EncodeToString([]byte("keyring")),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if actualSigner != signer {
		t.Fatalf("signer = %q, want %q", actualSigner, signer)
	}
	wantDigest := "9e5c96d6a01e1a5e265f4f594b5e118c2e8a8108578c789136b40fc6dc02bc95"
	actualDigest := sha256.Sum256(plain)
	if hex.EncodeToString(actualDigest[:]) != wantDigest {
		t.Fatalf("verified cleartext digest = %x, want %s", actualDigest, wantDigest)
	}
	if bytes.HasSuffix(plain, []byte("\n")) {
		t.Fatal("verified cleartext retained a gpgv-specific structural LF")
	}
}

type platformInputStore struct {
	descriptor ArtifactDescriptor
	raw        []byte
}

func (store platformInputStore) Stat(context.Context, string) (cas.Object, error) {
	return cas.Object{
		Digest:    store.descriptor.Digest,
		MediaType: store.descriptor.MediaType,
		SizeBytes: store.descriptor.SizeBytes,
	}, nil
}

func (store platformInputStore) Get(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(store.raw)), nil
}

func (platformInputStore) Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error) {
	return cas.Object{}, errors.New("unexpected publish")
}

func TestExtractPlatformInputVerifiesGNURecordPadding(t *testing.T) {
	content := []byte("runtime harness")
	raw := platformInputTar(t, content)
	// GNU tar pads its output to a 20-block record boundary by default. The Go
	// tar reader stops after the two logical end markers and leaves this valid
	// object padding unread unless the caller drains the content stream.
	raw = append(raw, make([]byte, 1024)...)
	descriptor := platformInputDescriptor(raw)
	destination := filepath.Join(t.TempDir(), "tree")
	acquirer := PlatformAcquirer{Store: platformInputStore{descriptor: descriptor, raw: raw}}
	if err := acquirer.extractPlatformInput(context.Background(), descriptor, destination); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(destination, "helmr", "entry.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("extracted content = %q, want %q", got, content)
	}
}

func TestExtractPlatformInputRejectsNonZeroSuffix(t *testing.T) {
	raw := append(platformInputTar(t, []byte("runtime harness")), []byte("not tar padding")...)
	descriptor := platformInputDescriptor(raw)
	acquirer := PlatformAcquirer{Store: platformInputStore{descriptor: descriptor, raw: raw}}
	err := acquirer.extractPlatformInput(context.Background(), descriptor, filepath.Join(t.TempDir(), "tree"))
	if err == nil || !strings.Contains(err.Error(), "non-zero data after tar end") {
		t.Fatalf("non-zero suffix error = %v", err)
	}
}

func TestExtractPlatformInputRejectsShortObject(t *testing.T) {
	raw := append(platformInputTar(t, []byte("runtime harness")), make([]byte, 1024)...)
	descriptor := platformInputDescriptor(raw)
	acquirer := PlatformAcquirer{Store: platformInputStore{descriptor: descriptor, raw: raw[:len(raw)-1]}}
	err := acquirer.extractPlatformInput(context.Background(), descriptor, filepath.Join(t.TempDir(), "tree"))
	if err == nil || !strings.Contains(err.Error(), "bytes do not match policy") {
		t.Fatalf("short object error = %v", err)
	}
}

func TestExtractPlatformInputRejectsDeclaredSizeOverrun(t *testing.T) {
	raw := platformInputTar(t, []byte("runtime harness"))
	descriptor := platformInputDescriptor(raw)
	acquirer := PlatformAcquirer{
		Store: platformInputStore{descriptor: descriptor, raw: append(raw, 0)},
	}
	err := acquirer.extractPlatformInput(context.Background(), descriptor, filepath.Join(t.TempDir(), "tree"))
	if err == nil || !strings.Contains(err.Error(), "exceeds its declared size") {
		t.Fatalf("declared-size overrun error = %v", err)
	}
}

func TestRewriteRuntimeNodeInterpreterChangesOnlyTheExistingSegment(t *testing.T) {
	raw := buildTestELF64(t, testELF64Spec{
		machine:      elf.EM_X86_64,
		fileType:     elf.ET_DYN,
		interpreters: []string{upstreamNodeInterpreter},
		needed:       []string{"libc.so.6"},
	})
	path := filepath.Join(t.TempDir(), "node")
	if err := os.WriteFile(path, raw, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := rewriteRuntimeNodeInterpreter(path); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != len(raw) {
		t.Fatalf("rewritten Node.js size = %d, want %d", len(updated), len(raw))
	}
	object, err := elf.NewFile(bytes.NewReader(updated))
	if err != nil {
		t.Fatal(err)
	}
	defer object.Close()
	interpreter, exists, err := runtimeELFInterpreter(object)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || interpreter != runtimeNodeInterpreter {
		t.Fatalf("rewritten interpreter = (%q, %t)", interpreter, exists)
	}
	if err := rewriteRuntimeNodeInterpreter(path); err == nil {
		t.Fatal("already rewritten Node.js was accepted as an upstream input")
	}
}

func platformInputTar(t *testing.T, content []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	archiveWriter := tar.NewWriter(&raw)
	if err := archiveWriter.WriteHeader(&tar.Header{
		Name: "helmr/entry.mjs",
		Mode: 0o644,
		Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := archiveWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := archiveWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return raw.Bytes()
}

func platformInputDescriptor(raw []byte) ArtifactDescriptor {
	return ArtifactDescriptor{
		Digest:    fmt.Sprintf("sha256:%x", sha256.Sum256(raw)),
		MediaType: PlatformTreeInputMediaType,
		SizeBytes: int64(len(raw)),
	}
}

func TestNodeModuleABIScansConditionalHeaderForNumericDistributionDefault(t *testing.T) {
	header := strings.Join([]string{
		"#if defined(NODE_EMBEDDER_MODULE_VERSION)",
		"#define NODE_MODULE_VERSION NODE_EMBEDDER_MODULE_VERSION",
		"#else",
		"#define NODE_MODULE_VERSION 137",
		"#endif",
	}, "\n")
	path := filepath.Join(t.TempDir(), "node_version.h")
	if err := os.WriteFile(path, []byte(header), 0o600); err != nil {
		t.Fatal(err)
	}
	version, err := nodeModuleABI(path)
	if err != nil {
		t.Fatal(err)
	}
	if version != "137" {
		t.Fatalf("module ABI = %q, want 137", version)
	}
}

func TestNodeModuleABIRejectsMissingAmbiguousOrNonCanonicalDefinition(t *testing.T) {
	for _, test := range []struct {
		name   string
		header string
	}{
		{name: "missing", header: "#define NODE_MAJOR_VERSION 24\n"},
		{name: "symbolic only", header: "#define NODE_MODULE_VERSION NODE_EMBEDDER_MODULE_VERSION\n"},
		{name: "leading zero", header: "#define NODE_MODULE_VERSION 0137\n"},
		{name: "conflicting", header: "#define NODE_MODULE_VERSION 137\n#define NODE_MODULE_VERSION 138\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "node_version.h")
			if err := os.WriteFile(path, []byte(test.header), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := nodeModuleABI(path); err == nil {
				t.Fatal("invalid module ABI header succeeded")
			}
		})
	}
}

func TestConformanceFailureClassifiesOnlyVerifiedInvalidResultAsDeterministic(t *testing.T) {
	invalid := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		nil,
	)
	var deterministic interface {
		PlatformAcquisitionFailureReason() workerapi.PlatformAcquisitionFailureReason
	}
	if !errors.As(invalid, &deterministic) ||
		deterministic.PlatformAcquisitionFailureReason() !=
			workerapi.PlatformAcquisitionConformanceFailed {
		t.Fatalf("invalid result classification = %v", invalid)
	}

	infrastructure := conformanceFailure(errors.New("validator unavailable"), nil)
	if errors.As(infrastructure, &deterministic) {
		t.Fatalf("validator outage was terminalized: %v", infrastructure)
	}

	closeFailure := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		errors.New("snapshot close failed"),
	)
	if errors.As(closeFailure, &deterministic) {
		t.Fatalf("snapshot close failure was terminalized: %v", closeFailure)
	}
}
