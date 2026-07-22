package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	RuntimeIndexFormatVersion      = 0
	RuntimeDescriptorFormatVersion = 0
	RuntimeArtifactMediaType       = "application/vnd.helmr.runtime.v0+squashfs"
	maxRuntimeDocumentBytes        = 4096
	runtimeMountPath               = "/opt/helmr/runtime"
)

type RuntimeIndex struct {
	Architecture      RuntimeArchitecture `json:"architecture"`
	FormatVersion     int                 `json:"formatVersion"`
	RuntimeAPIVersion string              `json:"runtimeApiVersion"`
}

type RuntimeDescriptor struct {
	Architecture      RuntimeArchitecture `json:"architecture"`
	Digest            string              `json:"digest"`
	FormatVersion     int                 `json:"formatVersion"`
	MediaType         string              `json:"mediaType"`
	RuntimeAPIVersion string              `json:"runtimeApiVersion"`
	SizeBytes         int64               `json:"sizeBytes"`
}

func RuntimeDescriptorFromWire(value api.WorkerRuntimeDescriptor) (RuntimeDescriptor, error) {
	descriptor := RuntimeDescriptor{
		Architecture:      RuntimeArchitecture(value.Architecture),
		Digest:            value.Digest,
		FormatVersion:     value.FormatVersion,
		MediaType:         value.MediaType,
		RuntimeAPIVersion: value.RuntimeAPIVersion,
		SizeBytes:         value.SizeBytes,
	}
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return RuntimeDescriptor{}, err
	}
	return descriptor, nil
}

func RuntimeDescriptorWire(descriptor RuntimeDescriptor) (api.WorkerRuntimeDescriptor, error) {
	if err := ValidateRuntimeDescriptor(descriptor); err != nil {
		return api.WorkerRuntimeDescriptor{}, err
	}
	return api.WorkerRuntimeDescriptor{
		Architecture:      string(descriptor.Architecture),
		Digest:            descriptor.Digest,
		FormatVersion:     descriptor.FormatVersion,
		MediaType:         descriptor.MediaType,
		RuntimeAPIVersion: descriptor.RuntimeAPIVersion,
		SizeBytes:         descriptor.SizeBytes,
	}, nil
}

func RuntimeArchitectureFromGo(value string) (RuntimeArchitecture, error) {
	architecture, err := compute.RuntimeArchitectureFromGo(value)
	return RuntimeArchitecture(architecture), err
}

func RuntimeArchitectureGo(value RuntimeArchitecture) (string, error) {
	switch value {
	case ArchitectureAArch64:
		return "arm64", nil
	case ArchitectureX8664:
		return "amd64", nil
	default:
		return "", fmt.Errorf("runtime architecture %q is unsupported", value)
	}
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
	if index.FormatVersion != RuntimeIndexFormatVersion {
		return fmt.Errorf(
			"runtime index formatVersion = %d, want %d",
			index.FormatVersion,
			RuntimeIndexFormatVersion,
		)
	}
	if index.RuntimeAPIVersion != RuntimeAPIVersion {
		return fmt.Errorf(
			"runtime index runtimeApiVersion = %q, want %q",
			index.RuntimeAPIVersion,
			RuntimeAPIVersion,
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
	if descriptor.RuntimeAPIVersion != RuntimeAPIVersion {
		return fmt.Errorf(
			"runtime descriptor runtimeApiVersion = %q, want %q",
			descriptor.RuntimeAPIVersion,
			RuntimeAPIVersion,
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
