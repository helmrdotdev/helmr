package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ToolchainFormatVersion = 0
	ToolchainMediaType     = "application/vnd.helmr.standard-toolchain.v0+squashfs"

	maxToolchainBytes                = 4 << 10
	maxToolchainCatalogMembers       = 1024
	maxToolArtifactBytes       int64 = 4 << 30

	toolchainDigestDomain = "helmr.standard-toolchain.v0\x00"
)

type Toolchain struct {
	Architecture         RuntimeArchitecture `json:"architecture"`
	FormatVersion        int                 `json:"formatVersion"`
	ManagedRuntimeDigest string              `json:"managedRuntimeDigest"`
	ToolchainClosure     ArtifactDescriptor  `json:"toolchainClosure"`
}

func CanonicalToolchain(value Toolchain) ([]byte, error) {
	if err := validateToolchain(value); err != nil {
		return nil, err
	}
	return canonicalToolchainDocument(value)
}

func ParseToolchain(raw []byte) (Toolchain, error) {
	if len(raw) == 0 || len(raw) > maxToolchainBytes {
		return Toolchain{}, fmt.Errorf(
			"toolchain size is outside [1,%d]",
			maxToolchainBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return Toolchain{}, fmt.Errorf("canonicalize toolchain: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return Toolchain{}, errors.New(
			"toolchain is not RFC 8785 canonical JSON",
		)
	}
	var value Toolchain
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Toolchain{}, fmt.Errorf("decode toolchain: %w", err)
	}
	if err := ensureEOF(decoder, "toolchain"); err != nil {
		return Toolchain{}, err
	}
	complete, err := CanonicalToolchain(value)
	if err != nil {
		return Toolchain{}, err
	}
	if !bytes.Equal(raw, complete) {
		return Toolchain{}, errors.New(
			"toolchain does not match the complete closed v0 shape",
		)
	}
	return value, nil
}

func ToolchainDigest(value Toolchain) (string, error) {
	canonical, err := CanonicalToolchain(value)
	if err != nil {
		return "", err
	}
	digest := domainDigest(toolchainDigestDomain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func SHA256DigestBytes(value string) ([]byte, error) {
	if !validToolDigest(value) {
		return nil, errors.New(
			"digest is not a lowercase SHA-256 digest",
		)
	}
	decoded, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil {
		return nil, errors.New(
			"digest is not a lowercase SHA-256 digest",
		)
	}
	return decoded, nil
}

func SHA256DigestString(value []byte) (string, error) {
	if len(value) != sha256.Size {
		return "", fmt.Errorf("digest is not %d bytes", sha256.Size)
	}
	return "sha256:" + hex.EncodeToString(value), nil
}

func validateToolchain(value Toolchain) error {
	if value.FormatVersion != ToolchainFormatVersion {
		return fmt.Errorf(
			"toolchain formatVersion = %d, want %d",
			value.FormatVersion,
			ToolchainFormatVersion,
		)
	}
	if err := ValidateRuntimeArchitecture(value.Architecture); err != nil {
		return fmt.Errorf("toolchain: %w", err)
	}
	if !validToolDigest(value.ManagedRuntimeDigest) {
		return errors.New(
			"toolchain managedRuntimeDigest is not a lowercase SHA-256 digest",
		)
	}
	return validateToolArtifact(
		value.ToolchainClosure,
		ToolchainMediaType,
		"toolchain closure",
	)
}

func validateToolArtifact(value ArtifactDescriptor, mediaType, label string) error {
	if !validToolDigest(value.Digest) {
		return fmt.Errorf("%s digest is not a lowercase SHA-256 digest", label)
	}
	if value.MediaType != mediaType {
		return fmt.Errorf(
			"%s mediaType = %q, want %q",
			label,
			value.MediaType,
			mediaType,
		)
	}
	if value.SizeBytes < 1 || value.SizeBytes > maxToolArtifactBytes {
		return fmt.Errorf(
			"%s sizeBytes is outside [1,%d]",
			label,
			maxToolArtifactBytes,
		)
	}
	return nil
}

func validToolDigest(value string) bool {
	return sha256DigestPattern.MatchString(value)
}

func canonicalToolchainDocument(value Toolchain) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode toolchain: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize toolchain: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > maxToolchainBytes {
		return nil, fmt.Errorf(
			"toolchain size is outside [1,%d]",
			maxToolchainBytes,
		)
	}
	return canonical, nil
}
