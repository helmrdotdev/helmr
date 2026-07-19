package deployment

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ManagerSelectorFormatVersion = 0
	ManagerCapsuleFormatVersion  = 0
	ManagerSelectorProfile       = "helmr.official-managers.v0"

	ManagerEntrypointNative = ManagerEntrypointKind("native")
	ManagerEntrypointNode   = ManagerEntrypointKind("node")

	ManagerTreeMediaType    = "application/vnd.helmr.package-manager.v0+squashfs"
	ManagerCapsuleMediaType = "application/vnd.helmr.package-manager-capsule.v0+json"
	ManagerClaimMediaType   = "application/vnd.helmr.package-manager-selector-claim.v0+json"

	maxManagerSelectorBytes     = 4 << 10
	maxManagerClaimBytes        = 4 << 10
	maxManagerCapsuleBytes      = 32 << 10
	maxManagerDistributionBytes = 256 << 20
	maxManagerCapsuleTreeBytes  = 512 << 20
	managerSelectorDigestDomain = "helmr.package-manager-selector.v0\x00"
	managerCapsuleDigestDomain  = "helmr.package-manager-capsule.v0\x00"
	managerSelectorClaimKeyRoot = "v0/claims/sha256/"
	managerCapsuleObjectKeyRoot = "v0/capsules/sha256/"
	managerCapsuleTreeKeyRoot   = "v0/trees/sha256/"
	managerBunEntrypoint        = "/opt/helmr/manager/bin/bun"
	managerNPMEntrypoint        = "/opt/helmr/manager/lib/npm/bin/npm-cli.js"
	managerBunReleaseOriginRoot = "https://github.com/oven-sh/bun/releases/download/"
	managerNPMReleaseOriginRoot = "https://registry.npmjs.org/npm/-/"
)

type ManagerEntrypointKind string

type ManagerSelector struct {
	Architecture   RuntimeArchitecture `json:"architecture"`
	FormatVersion  int                 `json:"formatVersion"`
	PackageManager PackageManager      `json:"packageManager"`
	Profile        string              `json:"profile"`
}

type ManagerClaim struct {
	Architecture         RuntimeArchitecture `json:"architecture"`
	FormatVersion        int                 `json:"formatVersion"`
	ManagerCapsuleDigest string              `json:"managerCapsuleDigest"`
	PackageManager       PackageManager      `json:"packageManager"`
}

type ManagerEntrypoint struct {
	Kind ManagerEntrypointKind `json:"kind"`
	Path string                `json:"path"`
}

type ManagerSource struct {
	Digest    string `json:"digest"`
	Origin    string `json:"origin"`
	SizeBytes int64  `json:"sizeBytes"`
}

type ManagerCapsule struct {
	Architecture   RuntimeArchitecture `json:"architecture"`
	Entrypoint     ManagerEntrypoint   `json:"entrypoint"`
	FormatVersion  int                 `json:"formatVersion"`
	PackageManager PackageManager      `json:"packageManager"`
	Source         ManagerSource       `json:"source"`
	Tree           ManagerArtifact     `json:"tree"`
}

func NewManagerSelector(
	manager PackageManager,
	architecture RuntimeArchitecture,
) ManagerSelector {
	return ManagerSelector{
		Architecture:   architecture,
		FormatVersion:  ManagerSelectorFormatVersion,
		PackageManager: manager,
		Profile:        ManagerSelectorProfile,
	}
}

func CanonicalManagerSelector(selector ManagerSelector) ([]byte, error) {
	if err := validateManagerSelector(selector); err != nil {
		return nil, err
	}
	return canonicalCapsuleDocument(
		selector,
		"manager selector",
		maxManagerSelectorBytes,
	)
}

func ParseManagerSelector(raw []byte) (ManagerSelector, error) {
	var selector ManagerSelector
	if err := parseCapsuleDocument(
		raw,
		&selector,
		"manager selector",
		maxManagerSelectorBytes,
	); err != nil {
		return ManagerSelector{}, err
	}
	if err := validateManagerSelector(selector); err != nil {
		return ManagerSelector{}, err
	}
	complete, err := CanonicalManagerSelector(selector)
	if err != nil {
		return ManagerSelector{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerSelector{}, errors.New(
			"manager selector does not match the complete canonical v0 shape",
		)
	}
	return selector, nil
}

func ManagerSelectorDigest(selector ManagerSelector) (string, error) {
	canonical, err := CanonicalManagerSelector(selector)
	if err != nil {
		return "", err
	}
	return capsuleDigest(managerSelectorDigestDomain, canonical), nil
}

func ManagerSelectorKey(selector ManagerSelector) (string, error) {
	digest, err := ManagerSelectorDigest(selector)
	if err != nil {
		return "", err
	}
	return managerDigestKey(managerSelectorClaimKeyRoot, digest)
}

func CanonicalManagerClaim(claim ManagerClaim) ([]byte, error) {
	if err := validateManagerClaim(claim); err != nil {
		return nil, err
	}
	return canonicalCapsuleDocument(claim, "manager claim", maxManagerClaimBytes)
}

func ParseManagerClaim(raw []byte) (ManagerClaim, error) {
	var claim ManagerClaim
	if err := parseCapsuleDocument(
		raw,
		&claim,
		"manager claim",
		maxManagerClaimBytes,
	); err != nil {
		return ManagerClaim{}, err
	}
	if err := validateManagerClaim(claim); err != nil {
		return ManagerClaim{}, err
	}
	complete, err := CanonicalManagerClaim(claim)
	if err != nil {
		return ManagerClaim{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerClaim{}, errors.New(
			"manager claim does not match the complete canonical v0 shape",
		)
	}
	return claim, nil
}

func CanonicalManagerCapsule(capsule ManagerCapsule) ([]byte, error) {
	if err := validateManagerCapsule(capsule); err != nil {
		return nil, err
	}
	return canonicalCapsuleDocument(
		capsule,
		"manager capsule",
		maxManagerCapsuleBytes,
	)
}

func ParseManagerCapsule(raw []byte) (ManagerCapsule, error) {
	var capsule ManagerCapsule
	if err := parseCapsuleDocument(
		raw,
		&capsule,
		"manager capsule",
		maxManagerCapsuleBytes,
	); err != nil {
		return ManagerCapsule{}, err
	}
	if err := validateManagerCapsule(capsule); err != nil {
		return ManagerCapsule{}, err
	}
	complete, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		return ManagerCapsule{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ManagerCapsule{}, errors.New(
			"manager capsule does not match the complete canonical v0 shape",
		)
	}
	return capsule, nil
}

func ManagerCapsuleDigest(capsule ManagerCapsule) (string, error) {
	canonical, err := CanonicalManagerCapsule(capsule)
	if err != nil {
		return "", err
	}
	return capsuleDigest(managerCapsuleDigestDomain, canonical), nil
}

func ManagerCapsuleKey(digest string) (string, error) {
	return managerDigestKey(managerCapsuleObjectKeyRoot, digest)
}

func ManagerTreeKey(digest string) (string, error) {
	return managerDigestKey(managerCapsuleTreeKeyRoot, digest)
}

func ValidateManagerAuthority(
	selector ManagerSelector,
	claim ManagerClaim,
	capsule ManagerCapsule,
) error {
	if err := validateManagerSelector(selector); err != nil {
		return err
	}
	if err := validateManagerClaim(claim); err != nil {
		return err
	}
	if err := validateManagerCapsule(capsule); err != nil {
		return err
	}
	if selector.Architecture != claim.Architecture ||
		selector.Architecture != capsule.Architecture ||
		selector.PackageManager != claim.PackageManager ||
		selector.PackageManager != capsule.PackageManager {
		return errors.New("manager selector, claim, and capsule do not match")
	}
	digest, err := ManagerCapsuleDigest(capsule)
	if err != nil {
		return err
	}
	if claim.ManagerCapsuleDigest != digest {
		return errors.New("manager claim does not name the canonical capsule")
	}
	return nil
}

func validateManagerSelector(selector ManagerSelector) error {
	if selector.FormatVersion != ManagerSelectorFormatVersion {
		return fmt.Errorf(
			"manager selector formatVersion = %d, want %d",
			selector.FormatVersion,
			ManagerSelectorFormatVersion,
		)
	}
	if !validArchitecture(selector.Architecture) {
		return fmt.Errorf(
			"manager selector architecture %q is unsupported",
			selector.Architecture,
		)
	}
	if err := validateManagerPackage(selector.PackageManager); err != nil {
		return err
	}
	if selector.Profile != ManagerSelectorProfile {
		return fmt.Errorf(
			"manager selector profile = %q, want %q",
			selector.Profile,
			ManagerSelectorProfile,
		)
	}
	return nil
}

func validateManagerClaim(claim ManagerClaim) error {
	if claim.FormatVersion != ManagerSelectorFormatVersion {
		return fmt.Errorf(
			"manager claim formatVersion = %d, want %d",
			claim.FormatVersion,
			ManagerSelectorFormatVersion,
		)
	}
	if !validArchitecture(claim.Architecture) {
		return fmt.Errorf(
			"manager claim architecture %q is unsupported",
			claim.Architecture,
		)
	}
	if err := validateManagerPackage(claim.PackageManager); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(claim.ManagerCapsuleDigest) {
		return errors.New(
			"manager claim managerCapsuleDigest is not a lowercase SHA-256 digest",
		)
	}
	return nil
}

func validateManagerCapsule(capsule ManagerCapsule) error {
	if capsule.FormatVersion != ManagerCapsuleFormatVersion {
		return fmt.Errorf(
			"manager capsule formatVersion = %d, want %d",
			capsule.FormatVersion,
			ManagerCapsuleFormatVersion,
		)
	}
	if !validArchitecture(capsule.Architecture) {
		return fmt.Errorf(
			"manager capsule architecture %q is unsupported",
			capsule.Architecture,
		)
	}
	if err := validateManagerPackage(capsule.PackageManager); err != nil {
		return err
	}
	expectedKind, expectedPath, expectedOrigin, err := managerDistribution(
		capsule.PackageManager,
		capsule.Architecture,
	)
	if err != nil {
		return err
	}
	if capsule.Entrypoint.Kind != expectedKind ||
		capsule.Entrypoint.Path != expectedPath {
		return errors.New(
			"manager capsule entrypoint does not match the official distribution",
		)
	}
	if !validManagerEntrypoint(capsule.Entrypoint.Path) {
		return errors.New("manager capsule entrypoint is not canonical")
	}
	if !sha256DigestPattern.MatchString(capsule.Source.Digest) {
		return errors.New(
			"manager capsule source digest is not a lowercase SHA-256 digest",
		)
	}
	if capsule.Source.Origin != expectedOrigin {
		return errors.New(
			"manager capsule source origin does not match the official distribution",
		)
	}
	if capsule.Source.SizeBytes < 1 ||
		capsule.Source.SizeBytes > maxManagerDistributionBytes {
		return fmt.Errorf(
			"manager capsule source sizeBytes is outside [1,%d]",
			maxManagerDistributionBytes,
		)
	}
	if err := validateManagerArtifact(
		capsule.Tree,
		ManagerTreeMediaType,
		maxManagerCapsuleTreeBytes,
		"manager capsule tree",
	); err != nil {
		return err
	}
	return nil
}

func managerDistribution(
	manager PackageManager,
	architecture RuntimeArchitecture,
) (ManagerEntrypointKind, string, string, error) {
	switch manager.Name {
	case PackageManagerBun:
		var asset string
		switch architecture {
		case ArchitectureAArch64:
			asset = "bun-linux-aarch64.zip"
		case ArchitectureX8664:
			asset = "bun-linux-x64-baseline.zip"
		default:
			return "", "", "", fmt.Errorf(
				"manager distribution architecture %q is unsupported",
				architecture,
			)
		}
		origin := managerBunReleaseOriginRoot +
			"bun-v" + manager.Version + "/" + asset
		return ManagerEntrypointNative, managerBunEntrypoint, origin, nil
	case PackageManagerNPM:
		if !validArchitecture(architecture) {
			return "", "", "", fmt.Errorf(
				"manager distribution architecture %q is unsupported",
				architecture,
			)
		}
		origin := managerNPMReleaseOriginRoot +
			"npm-" + manager.Version + ".tgz"
		return ManagerEntrypointNode, managerNPMEntrypoint, origin, nil
	default:
		return "", "", "", fmt.Errorf(
			"package manager %q is unsupported",
			manager.Name,
		)
	}
}

func validManagerEntrypoint(value string) bool {
	return utf8.ValidString(value) &&
		!strings.ContainsRune(value, 0) &&
		path.IsAbs(value) &&
		path.Clean(value) == value &&
		strings.HasPrefix(value, "/opt/helmr/manager/") &&
		value != "/opt/helmr/manager"
}

func canonicalCapsuleDocument(
	value any,
	label string,
	maxBytes int,
) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", label, err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if len(canonical) == 0 || len(canonical) > maxBytes {
		return nil, fmt.Errorf("%s size is outside [1,%d]", label, maxBytes)
	}
	return canonical, nil
}

func parseCapsuleDocument(
	raw []byte,
	destination any,
	label string,
	maxBytes int,
) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return fmt.Errorf("%s size is outside [1,%d]", label, maxBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return fmt.Errorf("canonicalize %s: %w", label, err)
	}
	if !bytes.Equal(raw, canonical) {
		return fmt.Errorf("%s is not RFC 8785 canonical JSON", label)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("decode %s: %w", label, err)
	}
	return ensureEOF(decoder, label)
}

func capsuleDigest(domain string, canonical []byte) string {
	digest := domainDigest(domain, canonical)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func managerDigestKey(root, digest string) (string, error) {
	if !sha256DigestPattern.MatchString(digest) {
		return "", errors.New("manager store digest is not a lowercase SHA-256 digest")
	}
	return root + strings.TrimPrefix(digest, "sha256:"), nil
}
