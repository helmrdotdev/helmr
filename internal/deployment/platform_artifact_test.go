package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
)

func TestVerifyRetainedNodeSourceUsesPinnedReleaseKey(t *testing.T) {
	source := []byte("node distribution")
	sourceDigest := sha256.Sum256(source)
	filename := "node-v24.16.0-linux-x64.tar.xz"
	checksums := []byte(hex.EncodeToString(sourceDigest[:]) + "  " + filename + "\n")

	entity, err := openpgp.NewEntity("Helmr Test", "", "test@helmr.dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	var keyring bytes.Buffer
	if err := entity.Serialize(&keyring); err != nil {
		t.Fatal(err)
	}
	var signed bytes.Buffer
	writer, err := clearsign.Encode(&signed, entity.PrivateKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(checksums); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	memory := newMemoryArtifact()
	memory.addDirectory("helmr")
	memory.addDirectory("helmr/upstream")
	memory.addFile("helmr/upstream/SHASUMS256.txt", checksums, 0644)
	memory.addFile("helmr/upstream/SHASUMS256.txt.asc", signed.Bytes(), 0644)
	artifact, err := inspectArtifact(
		context.Background(),
		memory,
		runtimeArtifact,
		maxRuntimeLogicalBytes,
		squashFSPhysicalAlign,
	)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.ToUpper(hex.EncodeToString(entity.PrimaryKey.Fingerprint))
	integrity := PlatformIntegrity{
		Identity: fingerprint,
		Evidence: []PlatformEvidenceFile{
			{Path: "helmr/upstream/SHASUMS256.txt"},
			{Path: "helmr/upstream/SHASUMS256.txt.asc"},
			{Path: "helmr/upstream/runtime-inputs.json"},
			{Path: "helmr/upstream/source"},
		},
		Source: PlatformSource{
			Digest:    "sha256:" + hex.EncodeToString(sourceDigest[:]),
			Origin:    NodeReleaseOrigin + "v24.16.0/" + filename,
			SizeBytes: int64(len(source)),
		},
	}
	expectation := PlatformArtifactExpectation{
		IntegrityIdentities: []string{fingerprint},
		NodeReleaseKeyring:  base64.StdEncoding.EncodeToString(keyring.Bytes()),
		SourceOrigin:        integrity.Source.Origin,
	}
	if err := verifyRetainedNodeSource(
		context.Background(),
		artifact,
		bytes.NewReader(source),
		integrity,
		expectation,
	); err != nil {
		t.Fatal(err)
	}
	if err := verifyRetainedNodeSource(
		context.Background(),
		artifact,
		bytes.NewReader([]byte("tampered source")),
		integrity,
		expectation,
	); err == nil {
		t.Fatal("tampered Node distribution was accepted")
	}
}

func TestValidateRetainedSourceEvidenceBindsSourceDescriptor(t *testing.T) {
	integrity := PlatformIntegrity{
		Evidence: []PlatformEvidenceFile{{
			Path:      "helmr/upstream/source",
			Digest:    "sha256:source",
			SizeBytes: 42,
		}},
		Source: PlatformSource{
			Digest:    "sha256:source",
			SizeBytes: 42,
		},
	}
	if err := validateRetainedSourceEvidence(integrity); err != nil {
		t.Fatal(err)
	}
	integrity.Source.Digest = "sha256:tampered"
	if err := validateRetainedSourceEvidence(integrity); err == nil {
		t.Fatal("mismatched retained source digest was accepted")
	}
	integrity.Source.Digest = "sha256:source"
	integrity.Source.SizeBytes++
	if err := validateRetainedSourceEvidence(integrity); err == nil {
		t.Fatal("mismatched retained source size was accepted")
	}
}
