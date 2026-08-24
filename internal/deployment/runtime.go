package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
)

const (
	NodeNoStripTypes             = "--no-strip-types"
	NodeNoExperimentalStripTypes = "--no-experimental-strip-types"
)

const (
	RuntimeDescriptorFormatVersion = 0
	RuntimeMetadataFormatVersion   = 0
	RuntimeArtifactMediaType       = "application/vnd.helmr.runtime.v0+squashfs"
	maxRuntimeDocumentBytes        = 4096
	runtimeMountPath               = "/opt/helmr/runtime"
)

type RuntimeIndex struct {
	Architecture    RuntimeArchitecture `json:"architecture"`
	RuntimeContract string              `json:"runtimeContract"`
}

type RuntimeDescriptor struct {
	Architecture    RuntimeArchitecture `json:"architecture"`
	Digest          string              `json:"digest"`
	FormatVersion   int                 `json:"formatVersion"`
	MediaType       string              `json:"mediaType"`
	RuntimeContract string              `json:"runtimeContract"`
	SizeBytes       int64               `json:"sizeBytes"`
}

type RuntimeMetadata struct {
	Architecture     RuntimeArchitecture `json:"architecture"`
	FormatVersion    int                 `json:"formatVersion"`
	NodeVersion      string              `json:"nodeVersion"`
	ProgramNodeFlags []string            `json:"programNodeFlags"`
	RuntimeContract  string              `json:"runtimeContract"`
}

func ParseRuntimeMetadata(raw []byte) (RuntimeMetadata, error) {
	var metadata RuntimeMetadata
	if err := parseRuntimeDocument(raw, "runtime metadata", &metadata); err != nil {
		return RuntimeMetadata{}, err
	}
	if err := ValidateRuntimeMetadata(metadata); err != nil {
		return RuntimeMetadata{}, err
	}
	canonical, err := CanonicalRuntimeMetadata(metadata)
	if err != nil {
		return RuntimeMetadata{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return RuntimeMetadata{}, errors.New("runtime metadata does not match the complete canonical v0 shape")
	}
	metadata.ProgramNodeFlags = append([]string(nil), metadata.ProgramNodeFlags...)
	return metadata, nil
}

func CanonicalRuntimeMetadata(metadata RuntimeMetadata) ([]byte, error) {
	if err := ValidateRuntimeMetadata(metadata); err != nil {
		return nil, err
	}
	return canonicalRuntimeDocument(metadata, "runtime metadata")
}

func ValidateRuntimeMetadata(metadata RuntimeMetadata) error {
	if metadata.FormatVersion != RuntimeMetadataFormatVersion {
		return fmt.Errorf(
			"runtime metadata formatVersion = %d, want %d",
			metadata.FormatVersion,
			RuntimeMetadataFormatVersion,
		)
	}
	if !validArchitecture(metadata.Architecture) {
		return fmt.Errorf("runtime metadata architecture %q is unsupported", metadata.Architecture)
	}
	if metadata.RuntimeContract != RuntimeContract {
		return fmt.Errorf(
			"runtime metadata runtimeContract = %q, want %q",
			metadata.RuntimeContract,
			RuntimeContract,
		)
	}
	expectedFlags, err := NodeProgramFlags(metadata.NodeVersion)
	if err != nil {
		return fmt.Errorf("runtime metadata: %w", err)
	}
	if !slices.Equal(metadata.ProgramNodeFlags, expectedFlags) {
		return errors.New("runtime metadata Program Node flags do not match the Node ABI contract")
	}
	return nil
}

func NodeProgramFlags(version string) ([]string, error) {
	major, minor, patch, ok := parseReleaseVersion(version)
	if !ok {
		return nil, fmt.Errorf("the Node.js version %q is not an exact release", version)
	}
	if major == 24 && compareReleaseVersion(major, minor, patch, "24.12.0") >= 0 {
		return []string{NodeNoStripTypes, "--enable-source-maps"}, nil
	}
	if major == 22 || major == 24 {
		return []string{NodeNoExperimentalStripTypes, "--enable-source-maps"}, nil
	}
	return nil, fmt.Errorf("the Node.js version %q has no program launch contract", version)
}

func RuntimeArchitectureFromGo(value string) (RuntimeArchitecture, error) {
	architecture, err := runtimeid.ArchitectureFromGo(value)
	return RuntimeArchitecture(architecture), err
}

func RuntimeArchitectureGo(value RuntimeArchitecture) (string, error) {
	if value == ArchitectureX8664 {
		return "amd64", nil
	}
	return "", fmt.Errorf("runtime architecture %q is unsupported", value)
}

func ValidateRuntimeArchitecture(value RuntimeArchitecture) error {
	if !validArchitecture(value) {
		return fmt.Errorf("runtime architecture %q is unsupported", value)
	}
	return nil
}

func RuntimeDigestBytes(value string) ([]byte, error) {
	if len(value) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(value, "sha256:") {
		return nil, fmt.Errorf("runtime digest is not a lowercase SHA-256 digest")
	}
	digest, err := hex.DecodeString(value[len("sha256:"):])
	if err != nil || len(digest) != sha256.Size {
		return nil, fmt.Errorf("runtime digest is not a lowercase SHA-256 digest")
	}
	return digest, nil
}

func RuntimeDigestString(value []byte) (string, error) {
	if len(value) != sha256.Size {
		return "", fmt.Errorf("runtime digest is not %d bytes", sha256.Size)
	}
	return "sha256:" + hex.EncodeToString(value), nil
}

func ParseRuntimeIndex(raw []byte) (RuntimeIndex, error) {
	var index RuntimeIndex
	if err := parseRuntimeDocument(raw, "runtime index", &index); err != nil {
		return RuntimeIndex{}, err
	}
	if err := ValidateRuntimeIndex(index); err != nil {
		return RuntimeIndex{}, err
	}
	canonical, err := CanonicalRuntimeIndex(index)
	if err != nil {
		return RuntimeIndex{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return RuntimeIndex{}, fmt.Errorf("runtime index does not match the complete canonical v0 shape")
	}
	return index, nil
}

func CanonicalRuntimeIndex(index RuntimeIndex) ([]byte, error) {
	if err := ValidateRuntimeIndex(index); err != nil {
		return nil, err
	}
	return canonicalRuntimeDocument(index, "runtime index")
}

func ValidateRuntimeIndex(index RuntimeIndex) error {
	if index.RuntimeContract != RuntimeContract {
		return fmt.Errorf(
			"runtime index runtimeContract = %q, want %q",
			index.RuntimeContract,
			RuntimeContract,
		)
	}
	if !validArchitecture(index.Architecture) {
		return fmt.Errorf("runtime index architecture %q is unsupported", index.Architecture)
	}
	return nil
}

func ParseRuntimeDescriptor(raw []byte) (RuntimeDescriptor, error) {
	var descriptor RuntimeDescriptor
	if err := parseRuntimeDocument(raw, "runtime descriptor", &descriptor); err != nil {
		return RuntimeDescriptor{}, err
	}
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return RuntimeDescriptor{}, err
	}
	canonical, err := CanonicalRuntimeDescriptor(descriptor)
	if err != nil {
		return RuntimeDescriptor{}, err
	}
	if !bytes.Equal(raw, canonical) {
		return RuntimeDescriptor{}, fmt.Errorf(
			"runtime descriptor does not match the complete canonical v0 shape",
		)
	}
	return descriptor, nil
}

func CanonicalRuntimeDescriptor(descriptor RuntimeDescriptor) ([]byte, error) {
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return nil, err
	}
	return canonicalRuntimeDocument(descriptor, "runtime descriptor")
}

func ValidateRuntimeDescriptor(descriptor RuntimeDescriptor) error {
	if descriptor.FormatVersion != RuntimeDescriptorFormatVersion {
		return fmt.Errorf(
			"runtime descriptor formatVersion = %d, want %d",
			descriptor.FormatVersion,
			RuntimeDescriptorFormatVersion,
		)
	}
	if !validArchitecture(descriptor.Architecture) {
		return fmt.Errorf("runtime descriptor architecture %q is unsupported", descriptor.Architecture)
	}
	if !sha256DigestPattern.MatchString(descriptor.Digest) {
		return fmt.Errorf("runtime descriptor digest is not a lowercase SHA-256 digest")
	}
	if descriptor.MediaType != RuntimeArtifactMediaType {
		return fmt.Errorf(
			"runtime descriptor mediaType = %q, want %q",
			descriptor.MediaType,
			RuntimeArtifactMediaType,
		)
	}
	if descriptor.RuntimeContract != RuntimeContract {
		return fmt.Errorf(
			"runtime descriptor runtimeContract = %q, want %q",
			descriptor.RuntimeContract,
			RuntimeContract,
		)
	}
	if descriptor.SizeBytes < 1 || descriptor.SizeBytes > maxJSONSafeInteger {
		return fmt.Errorf("runtime descriptor sizeBytes is not a positive JavaScript-safe integer")
	}
	return nil
}

func parseRuntimeDocument(raw []byte, name string, destination any) error {
	if len(raw) == 0 || len(raw) > maxRuntimeDocumentBytes {
		return fmt.Errorf("%s size is outside [1,%d]", name, maxRuntimeDocumentBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize %s: %w", name, err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not RFC 8785 canonical JSON", name)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return ensureEOF(decoder, name)
}

func canonicalRuntimeDocument(value any, name string) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", name, err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s: %w", name, err)
	}
	if len(canonical) == 0 || len(canonical) > maxRuntimeDocumentBytes {
		return nil, fmt.Errorf("%s size is outside [1,%d]", name, maxRuntimeDocumentBytes)
	}
	return canonical, nil
}
