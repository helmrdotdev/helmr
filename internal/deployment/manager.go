package deployment

import (
	"fmt"
)

const (
	ManagerAdapterVersion = "helmr.manager.v0"

	ManagerEntrypointNative = ManagerEntrypointKind("native")
	ManagerEntrypointNode   = ManagerEntrypointKind("node")

	ManagerTreeMediaType = "application/vnd.helmr.package-manager.v0+squashfs"

	maxManagerDistributionBytes  = 256 << 20
	maxManagerTreeBytes          = 512 << 20
	managerBunEntrypoint         = "/opt/helmr/manager/bin/bun"
	managerNativeLauncherTarget  = "../../runtime/bin/native-manager"
	managerNPMEntrypoint         = "/opt/helmr/manager/lib/npm/bin/npm-cli.js"
	managerPNPMEntrypoint        = "/opt/helmr/manager/lib/pnpm/bin/pnpm.cjs"
	managerBunReleaseOriginRoot  = "https://github.com/oven-sh/bun/releases/download/"
	managerNPMReleaseOriginRoot  = "https://registry.npmjs.org/npm/-/"
	managerPNPMReleaseOriginRoot = "https://registry.npmjs.org/pnpm/-/"
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
	case PackageManagerPNPM:
		origin := managerPNPMReleaseOriginRoot +
			"pnpm-" + manager.Version + ".tgz"
		return ManagerEntrypointNode, managerPNPMEntrypoint, origin, nil
	default:
		return "", "", "", fmt.Errorf(
			"package manager %q is unsupported",
			manager.Name,
		)
	}
}

func ManagerSourceOrigin(manager PackageManager) (string, error) {
	_, _, origin, err := managerDistribution(manager)
	return origin, err
}

// ManagerInvocation returns the canonical argv for executing a verified
// Manager inside the isolated Runtime + Manager filesystem contract.
func ManagerInvocation(
	manager PackageManager,
	entrypoint ManagerEntrypoint,
	arguments ...string,
) ([]string, error) {
	expectedKind, expectedPath, _, err := managerDistribution(manager)
	if err != nil {
		return nil, err
	}
	if entrypoint.Kind != expectedKind || entrypoint.Path != expectedPath {
		return nil, fmt.Errorf("manager entrypoint does not match its family")
	}
	var invocation []string
	switch entrypoint.Kind {
	case ManagerEntrypointNode:
		invocation = []string{runtimeMountPath + "/bin/node", entrypoint.Path}
	case ManagerEntrypointNative:
		invocation = []string{entrypoint.Path}
	default:
		return nil, fmt.Errorf("manager entrypoint kind %q is unsupported", entrypoint.Kind)
	}
	return append(invocation, arguments...), nil
}
