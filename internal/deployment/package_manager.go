package deployment

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
)

const (
	PackageManagerBun = PackageManagerName("bun")
	PackageManagerNPM = PackageManagerName("npm")

	maxPackageManagerVersionBytes = 64
	sha512DigestBytes             = 64
)

var packageManagerVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

type PackageManagerName string

type PackageManager struct {
	Name    PackageManagerName `json:"name"`
	Version string             `json:"version"`
}

func validateManagerPackage(manager PackageManager) error {
	if manager.Name != PackageManagerBun && manager.Name != PackageManagerNPM {
		return fmt.Errorf("package manager name %q is unsupported", manager.Name)
	}
	if len(manager.Version) == 0 ||
		len(manager.Version) > maxPackageManagerVersionBytes ||
		!packageManagerVersionPattern.MatchString(manager.Version) {
		return fmt.Errorf(
			"package manager version %q is not an admitted SemVer",
			manager.Version,
		)
	}
	return nil
}

func validatePackageIntegrity(integrity string) error {
	const prefix = "sha512-"
	if !strings.HasPrefix(integrity, prefix) {
		return fmt.Errorf("integrity is not a canonical SHA-512 SRI value")
	}
	encoded := strings.TrimPrefix(integrity, prefix)
	digest, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil ||
		len(digest) != sha512DigestBytes ||
		base64.StdEncoding.EncodeToString(digest) != encoded {
		return fmt.Errorf("integrity is not a canonical SHA-512 SRI value")
	}
	return nil
}
