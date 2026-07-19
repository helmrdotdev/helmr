package deployment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	DependencyIndexFormatVersion  = 0
	DependencyCacheFormatVersion  = 0
	DependencyMaterializerVersion = "helmr.dependencies.v0"
	PackageManagerBun             = PackageManagerName("bun")
	PackageManagerNPM             = PackageManagerName("npm")
	maxDependencyIndexSizeBytes   = 4096
	maxPackageManagerVersionBytes = 64
	dependencyCacheKeyDomain      = "helmr.deployment-program-dependencies-cache.v0\x00"
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
	DependencyPlanDigest  string              `json:"dependencyPlanDigest"`
	PackageManager        PackageManager      `json:"packageManager"`
	Lockfile              DependencyLockfile  `json:"lockfile"`
	LocalManifestsDigest  string              `json:"localManifestsDigest"`
	PackageGraphDigest    string              `json:"packageGraphDigest"`
	PackageGraphSizeBytes int64               `json:"packageGraphSizeBytes"`
	MaterializerVersion   string              `json:"materializerVersion"`
	RuntimeDigest         string              `json:"runtimeDigest"`
	Architecture          RuntimeArchitecture `json:"architecture"`
}

type DependencyCacheInput struct {
	FormatVersion        int                 `json:"formatVersion"`
	DependencyPlanDigest string              `json:"dependencyPlanDigest"`
	PackageManager       PackageManager      `json:"packageManager"`
	Lockfile             DependencyLockfile  `json:"lockfile"`
	LocalManifestsDigest string              `json:"localManifestsDigest"`
	MaterializerVersion  string              `json:"materializerVersion"`
	RuntimeDigest        string              `json:"runtimeDigest"`
	Architecture         RuntimeArchitecture `json:"architecture"`
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
	if err := validateDependencyInputs(
		index.DependencyPlanDigest,
		index.PackageManager,
		index.Lockfile,
		index.LocalManifestsDigest,
		index.MaterializerVersion,
		index.RuntimeDigest,
		index.Architecture,
		"dependency index",
	); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(index.PackageGraphDigest) {
		return fmt.Errorf("dependency index packageGraphDigest is not a lowercase SHA-256 digest")
	}
	if index.PackageGraphSizeBytes < 1 || index.PackageGraphSizeBytes > maxProgramFileSizeBytes {
		return fmt.Errorf("dependency index packageGraphSizeBytes is outside [1,%d]", maxProgramFileSizeBytes)
	}
	return nil
}

func CanonicalDependencyCacheInput(input DependencyCacheInput) ([]byte, error) {
	if err := ValidateDependencyCacheInput(input); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode dependency cache input: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize dependency cache input: %w", err)
	}
	return canonical, nil
}

func DependencyCacheKey(input DependencyCacheInput) (string, error) {
	canonical, err := CanonicalDependencyCacheInput(input)
	if err != nil {
		return "", err
	}
	digest := domainDigest(dependencyCacheKeyDomain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func ValidateDependencyCacheInput(input DependencyCacheInput) error {
	if input.FormatVersion != DependencyCacheFormatVersion {
		return fmt.Errorf(
			"dependency cache input formatVersion = %d, want %d",
			input.FormatVersion,
			DependencyCacheFormatVersion,
		)
	}
	return validateDependencyInputs(
		input.DependencyPlanDigest,
		input.PackageManager,
		input.Lockfile,
		input.LocalManifestsDigest,
		input.MaterializerVersion,
		input.RuntimeDigest,
		input.Architecture,
		"dependency cache input",
	)
}

func validateDependencyInputs(
	dependencyPlanDigest string,
	manager PackageManager,
	lockfile DependencyLockfile,
	localManifestsDigest string,
	materializerVersion string,
	runtimeDigest string,
	architecture RuntimeArchitecture,
	label string,
) error {
	if !sha256DigestPattern.MatchString(dependencyPlanDigest) {
		return fmt.Errorf("%s dependencyPlanDigest is not a lowercase SHA-256 digest", label)
	}
	if manager.Name != PackageManagerBun && manager.Name != PackageManagerNPM {
		return fmt.Errorf("%s packageManager.name %q is unsupported", label, manager.Name)
	}
	if len(manager.Version) == 0 ||
		len(manager.Version) > maxPackageManagerVersionBytes ||
		!packageManagerVersionPattern.MatchString(manager.Version) {
		return fmt.Errorf("%s packageManager.version %q is not an admitted SemVer", label, manager.Version)
	}
	wantLockfile := "bun.lock"
	if manager.Name == PackageManagerNPM {
		wantLockfile = "package-lock.json"
	}
	if lockfile.Name != wantLockfile {
		return fmt.Errorf("%s lockfile.name = %q, want %q", label, lockfile.Name, wantLockfile)
	}
	if !sha256DigestPattern.MatchString(lockfile.Digest) {
		return fmt.Errorf("%s lockfile.digest is not a lowercase SHA-256 digest", label)
	}
	if !sha256DigestPattern.MatchString(localManifestsDigest) {
		return fmt.Errorf("%s localManifestsDigest is not a lowercase SHA-256 digest", label)
	}
	if materializerVersion != DependencyMaterializerVersion {
		return fmt.Errorf(
			"%s materializerVersion = %q, want %q",
			label,
			materializerVersion,
			DependencyMaterializerVersion,
		)
	}
	if !sha256DigestPattern.MatchString(runtimeDigest) {
		return fmt.Errorf("%s runtimeDigest is not a lowercase SHA-256 digest", label)
	}
	if !validArchitecture(architecture) {
		return fmt.Errorf("%s architecture %q is unsupported", label, architecture)
	}
	return nil
}
