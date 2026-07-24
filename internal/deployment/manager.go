package deployment

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

const (
	ManagerAdapterVersion = "helmr.manager.v0"

	ManagerEntrypointNative = ManagerEntrypointKind("native")
	ManagerEntrypointNode   = ManagerEntrypointKind("node")

	ManagerTreeMediaType = "application/vnd.helmr.package-manager.v0+squashfs"

	maxManagerDistributionBytes = 256 << 20
	maxManagerTreeBytes         = 512 << 20
	managerBunEntrypoint        = "/opt/helmr/manager/bin/bun"
	managerNPMEntrypoint        = "/opt/helmr/manager/lib/npm/bin/npm-cli.js"
	managerBunReleaseOriginRoot = "https://github.com/oven-sh/bun/releases/download/"
	managerNPMReleaseOriginRoot = "https://registry.npmjs.org/npm/-/"
)

type ManagerEntrypointKind string

type ManagerEntrypoint struct {
	Kind ManagerEntrypointKind `json:"kind"`
	Path string                `json:"path"`
}

type ManagerSource struct {
	Digest    string `json:"digest"`
	Origin    string `json:"origin"`
	SizeBytes int64  `json:"sizeBytes"`
}

type Manager struct {
	AdapterVersion string              `json:"adapterVersion"`
	Architecture   RuntimeArchitecture `json:"architecture"`
	Entrypoint     ManagerEntrypoint   `json:"entrypoint"`
	PackageManager PackageManager      `json:"packageManager"`
	Source         ManagerSource       `json:"source"`
	Tree           ArtifactDescriptor  `json:"tree"`
}

func validateManager(manager Manager) error {
	if manager.AdapterVersion != ManagerAdapterVersion {
		return fmt.Errorf(
			"Manager adapterVersion = %q, want %q",
			manager.AdapterVersion,
			ManagerAdapterVersion,
		)
	}
	if manager.Architecture != ArchitectureX8664 {
		return fmt.Errorf(
			"Manager architecture = %q, want %q",
			manager.Architecture,
			ArchitectureX8664,
		)
	}
	if err := validateManagerPackage(manager.PackageManager); err != nil {
		return err
	}
	expectedKind, expectedPath, expectedOrigin, err := managerDistribution(
		manager.PackageManager,
	)
	if err != nil {
		return err
	}
	if manager.Entrypoint.Kind != expectedKind ||
		manager.Entrypoint.Path != expectedPath {
		return errors.New("Manager entrypoint does not match the certified distribution")
	}
	if !validManagerEntrypoint(manager.Entrypoint.Path) {
		return errors.New("Manager entrypoint is not canonical")
	}
	if !sha256DigestPattern.MatchString(manager.Source.Digest) {
		return errors.New("Manager source digest is not a lowercase SHA-256 digest")
	}
	if manager.Source.Origin != expectedOrigin {
		return errors.New("Manager source origin does not match the certified distribution")
	}
	if manager.Source.SizeBytes < 1 ||
		manager.Source.SizeBytes > maxManagerDistributionBytes {
		return fmt.Errorf(
			"Manager source sizeBytes is outside [1,%d]",
			maxManagerDistributionBytes,
		)
	}
	if err := validateInputArtifact(
		manager.Tree,
		ManagerTreeMediaType,
		maxManagerTreeBytes,
		"Manager tree",
	); err != nil {
		return err
	}
	return nil
}

func managerDistribution(
	manager PackageManager,
) (ManagerEntrypointKind, string, string, error) {
	switch manager.Name {
	case PackageManagerBun:
		origin := managerBunReleaseOriginRoot +
			"bun-v" + manager.Version + "/bun-linux-x64-baseline.zip"
		return ManagerEntrypointNative, managerBunEntrypoint, origin, nil
	case PackageManagerNPM:
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
