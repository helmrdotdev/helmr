package deployment

import (
	"fmt"
	"regexp"
)

const (
	PackageManagerBun  = PackageManagerName("bun")
	PackageManagerNPM  = PackageManagerName("npm")
	PackageManagerPNPM = PackageManagerName("pnpm")

	maxPackageManagerVersionBytes = 64
)

var packageManagerVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

type PackageManagerName string

type PackageManager struct {
	Name    PackageManagerName `json:"name"`
	Version string             `json:"version"`
}

func validateManagerPackage(manager PackageManager) error {
	if manager.Name != PackageManagerBun &&
		manager.Name != PackageManagerNPM &&
		manager.Name != PackageManagerPNPM {
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
	major, minor, patch, ok := parseReleaseVersion(manager.Version)
	if !ok {
		return fmt.Errorf(
			"package manager version %q is not an admitted release",
			manager.Version,
		)
	}
	switch manager.Name {
	case PackageManagerNPM:
		if major != 11 || minor < 4 || minor == 4 && patch < 2 {
			return fmt.Errorf("npm version %q is outside >=11.4.2 <12", manager.Version)
		}
	case PackageManagerPNPM:
		if major != 11 || minor < 1 {
			return fmt.Errorf("pnpm version %q is outside >=11.1.0 <12", manager.Version)
		}
	case PackageManagerBun:
		if major != 1 || minor < 3 || minor == 3 && patch < 10 {
			return fmt.Errorf("bun version %q is outside >=1.3.10 <2", manager.Version)
		}
	}
	return nil
}

func parseReleaseVersion(value string) (int, int, int, bool) {
	match := packageManagerVersionPattern.FindStringSubmatch(value)
	if match == nil || match[4] != "" {
		return 0, 0, 0, false
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(value, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}
