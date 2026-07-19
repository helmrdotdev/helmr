package deployment

const (
	toolchainCatalogPath     = "/usr/lib/helmr/toolchain-release/catalog.json"
	toolchainBundlePath      = "/usr/lib/helmr/toolchain-release/catalog.sigstore.json"
	toolchainTrustedRootPath = "/usr/lib/helmr/toolchain-release/trusted-root.json"
)

func LoadToolchainCatalog() (*ToolchainCatalog, error) {
	catalogBytes, err := readReleaseFileOwned(
		toolchainCatalogPath,
		"standard-toolchain catalog",
		maxToolchainCatalogBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := readReleaseFileOwned(
		toolchainBundlePath,
		"standard-toolchain attestation bundle",
		maxReleaseBundleBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	trustedRootBytes, err := readReleaseFileOwned(
		toolchainTrustedRootPath,
		"standard-toolchain trusted root",
		maxReleaseTrustedRootBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	return VerifyToolchainCatalog(catalogBytes, bundleBytes, trustedRootBytes)
}
