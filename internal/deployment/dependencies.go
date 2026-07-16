package deployment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	DependencyIndexFormatVersion  = 0
	DependencyMaterializerVersion = "helmr.dependencies.v0"
	PackageManagerBun             = PackageManagerName("bun")
	PackageManagerNPM             = PackageManagerName("npm")
	maxDependencyIndexSizeBytes   = 4096
	maxPackageManagerVersionBytes = 64
)

var packageManagerVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

type PackageManagerName string

type PackageManager struct {
	Name    PackageManagerName `json:"name"`
	Version string             `json:"version"`
}

type DependencyLockfile struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type DependencyIndex struct {
	FormatVersion         int                 `json:"formatVersion"`
	PackageManager        PackageManager      `json:"packageManager"`
	Lockfile              DependencyLockfile  `json:"lockfile"`
	LocalManifestsDigest  string              `json:"localManifestsDigest"`
	PackageGraphDigest    string              `json:"packageGraphDigest"`
	PackageGraphSizeBytes int64               `json:"packageGraphSizeBytes"`
	MaterializerVersion   string              `json:"materializerVersion"`
	RuntimeDigest         string              `json:"runtimeDigest"`
	Architecture          RuntimeArchitecture `json:"architecture"`
}

func ParseDependencyIndex(raw []byte) (DependencyIndex, error) {
	if len(raw) == 0 || len(raw) > maxDependencyIndexSizeBytes {
		return DependencyIndex{}, fmt.Errorf("dependency index size is outside [1,%d]", maxDependencyIndexSizeBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return DependencyIndex{}, fmt.Errorf("canonicalize dependency index: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return DependencyIndex{}, fmt.Errorf("dependency index is not RFC 8785 canonical JSON")
	}

	var index DependencyIndex
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&index); err != nil {
		return DependencyIndex{}, fmt.Errorf("decode dependency index: %w", err)
	}
	if err := ensureEOF(decoder, "dependency index"); err != nil {
		return DependencyIndex{}, err
	}
	if err := ValidateDependencyIndex(index); err != nil {
		return DependencyIndex{}, err
	}
	complete, err := CanonicalDependencyIndex(index)
	if err != nil {
		return DependencyIndex{}, err
	}
	if !bytes.Equal(raw, complete) {
		return DependencyIndex{}, fmt.Errorf("dependency index does not match the complete canonical v0 shape")
	}
	return index, nil
}

func CanonicalDependencyIndex(index DependencyIndex) ([]byte, error) {
	if err := ValidateDependencyIndex(index); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(index)
	if err != nil {
		return nil, fmt.Errorf("encode dependency index: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency index: %w", err)
	}
	if len(canonical) > maxDependencyIndexSizeBytes {
		return nil, fmt.Errorf("dependency index size is outside [1,%d]", maxDependencyIndexSizeBytes)
	}
	return canonical, nil
}

func ValidateDependencyIndex(index DependencyIndex) error {
	if index.FormatVersion != DependencyIndexFormatVersion {
		return fmt.Errorf("dependency index formatVersion = %d, want %d", index.FormatVersion, DependencyIndexFormatVersion)
	}
	if index.PackageManager.Name != PackageManagerBun && index.PackageManager.Name != PackageManagerNPM {
		return fmt.Errorf("dependency index packageManager.name %q is unsupported", index.PackageManager.Name)
	}
	if len(index.PackageManager.Version) == 0 || len(index.PackageManager.Version) > maxPackageManagerVersionBytes || !packageManagerVersionPattern.MatchString(index.PackageManager.Version) {
		return fmt.Errorf("dependency index packageManager.version %q is not an admitted SemVer", index.PackageManager.Version)
	}
	wantLockfile := "bun.lock"
	if index.PackageManager.Name == PackageManagerNPM {
		wantLockfile = "package-lock.json"
	}
	if index.Lockfile.Name != wantLockfile {
		return fmt.Errorf("dependency index lockfile.name = %q, want %q", index.Lockfile.Name, wantLockfile)
	}
	if !sha256DigestPattern.MatchString(index.Lockfile.Digest) {
		return fmt.Errorf("dependency index lockfile.digest is not a lowercase SHA-256 digest")
	}
	if !sha256DigestPattern.MatchString(index.LocalManifestsDigest) {
		return fmt.Errorf("dependency index localManifestsDigest is not a lowercase SHA-256 digest")
	}
	if !sha256DigestPattern.MatchString(index.PackageGraphDigest) {
		return fmt.Errorf("dependency index packageGraphDigest is not a lowercase SHA-256 digest")
	}
	if index.PackageGraphSizeBytes < 1 || index.PackageGraphSizeBytes > maxProgramFileSizeBytes {
		return fmt.Errorf("dependency index packageGraphSizeBytes is outside [1,%d]", maxProgramFileSizeBytes)
	}
	if index.MaterializerVersion != DependencyMaterializerVersion {
		return fmt.Errorf("dependency index materializerVersion = %q, want %q", index.MaterializerVersion, DependencyMaterializerVersion)
	}
	if !sha256DigestPattern.MatchString(index.RuntimeDigest) {
		return fmt.Errorf("dependency index runtimeDigest is not a lowercase SHA-256 digest")
	}
	if !validArchitecture(index.Architecture) {
		return fmt.Errorf("dependency index architecture %q is unsupported", index.Architecture)
	}
	return nil
}
