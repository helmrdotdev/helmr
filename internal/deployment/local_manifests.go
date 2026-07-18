package deployment

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	LocalManifestsFormatVersion = 0
	localManifestsDigestDomain  = "helmr.deployment-local-manifests.v0\x00"
)

type LocalManifestEntry struct {
	ManifestDigest string `json:"manifestDigest"`
	Path           string `json:"path"`
}

type LocalManifests struct {
	FormatVersion int                  `json:"formatVersion"`
	Entries       []LocalManifestEntry `json:"entries"`
}

func CanonicalLocalManifests(manifests LocalManifests) ([]byte, error) {
	if err := ValidateLocalManifests(manifests); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(manifests)
	if err != nil {
		return nil, fmt.Errorf("encode local manifests: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize local manifests: %w", err)
	}
	if len(canonical) > int(maxProgramFileSizeBytes) {
		return nil, fmt.Errorf("local manifests size is outside [1,%d]", maxProgramFileSizeBytes)
	}
	return canonical, nil
}

func LocalManifestsDigest(manifests LocalManifests) ([sha256.Size]byte, error) {
	canonical, err := CanonicalLocalManifests(manifests)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return domainDigest(localManifestsDigestDomain, canonical), nil
}

func ValidateLocalManifests(manifests LocalManifests) error {
	if manifests.FormatVersion != LocalManifestsFormatVersion {
		return fmt.Errorf(
			"local manifests formatVersion = %d, want %d",
			manifests.FormatVersion,
			LocalManifestsFormatVersion,
		)
	}
	if manifests.Entries == nil {
		return fmt.Errorf("local manifests entries must be an array")
	}
	if len(manifests.Entries) == 0 || manifests.Entries[0].Path != "." {
		return fmt.Errorf("local manifests entries must begin with exactly one root record")
	}

	paths := make(map[string]struct{}, len(manifests.Entries))
	for position, entry := range manifests.Entries {
		if !sha256DigestPattern.MatchString(entry.ManifestDigest) {
			return fmt.Errorf(
				"local manifests entries[%d].manifestDigest is not a lowercase SHA-256 digest",
				position,
			)
		}
		if position == 0 {
			if entry.Path != "." {
				return fmt.Errorf("local manifests root path = %q, want %q", entry.Path, ".")
			}
		} else {
			if entry.Path == "." {
				return fmt.Errorf("root path may only appear in the first local manifest")
			}
			if err := validatePackagePath(entry.Path, programMountPath, true); err != nil {
				return fmt.Errorf("local manifests entries[%d].path: %w", position, err)
			}
			if position > 1 && bytes.Compare(
				[]byte(manifests.Entries[position-1].Path),
				[]byte(entry.Path),
			) >= 0 {
				return fmt.Errorf("local manifests entries are not in canonical path order at position %d", position)
			}
		}
		if _, exists := paths[entry.Path]; exists {
			return fmt.Errorf("local manifests entries contains duplicate path %q", entry.Path)
		}
		for separator := strings.IndexByte(entry.Path, '/'); separator >= 0; {
			ancestor := entry.Path[:separator]
			if _, exists := paths[ancestor]; exists {
				return fmt.Errorf("local manifest paths %q and %q overlap", ancestor, entry.Path)
			}
			next := strings.IndexByte(entry.Path[separator+1:], '/')
			if next < 0 {
				break
			}
			separator += next + 1
		}
		paths[entry.Path] = struct{}{}
	}
	return nil
}
