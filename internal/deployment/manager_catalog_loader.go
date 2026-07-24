package deployment

const (
	managerCatalogPath     = "/usr/lib/helmr/manager-release/catalog.json"
	managerBundlePath      = "/usr/lib/helmr/manager-release/catalog.sigstore.json"
	managerTrustedRootPath = "/usr/lib/helmr/manager-release/trusted-root.json"
)

func LoadManagerCatalog() (*ManagerCatalog, error) {
	catalogBytes, err := readReleaseFileOwned(
		managerCatalogPath,
		"Manager catalog",
		maxManagerCatalogBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := readReleaseFileOwned(
		managerBundlePath,
		"Manager attestation bundle",
		maxReleaseBundleBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	trustedRootBytes, err := readReleaseFileOwned(
		managerTrustedRootPath,
		"Manager trusted root",
		maxReleaseTrustedRootBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	return VerifyManagerCatalog(catalogBytes, bundleBytes, trustedRootBytes)
}
