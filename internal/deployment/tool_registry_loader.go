package deployment

const (
	toolRegistryPath = "/usr/lib/helmr/runtime-release/tool-registry.json"
	toolBundlePath   = "/usr/lib/helmr/runtime-release/tool-registry.sigstore.json"
)

func LoadToolRegistry() (*ToolRegistry, error) {
	registryBytes, err := readReleaseFileOwned(
		toolRegistryPath,
		"dependency tool registry",
		maxToolRegistryBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := readReleaseFileOwned(
		toolBundlePath,
		"dependency tool attestation bundle",
		maxReleaseBundleBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	trustedRootBytes, err := readReleaseFileOwned(
		runtimeTrustedRootPath,
		"release trusted root",
		maxReleaseTrustedRootBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	return VerifyToolRegistry(registryBytes, bundleBytes, trustedRootBytes)
}
