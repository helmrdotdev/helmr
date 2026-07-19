package deployment

import (
	"fmt"
	"io"
	"os"
)

const (
	runtimeCatalogPath     = "/usr/lib/helmr/runtime-release/catalog.json"
	runtimeBundlePath      = "/usr/lib/helmr/runtime-release/catalog.sigstore.json"
	runtimeTrustedRootPath = "/usr/lib/helmr/runtime-release/trusted-root.json"
)

// LoadRuntimeCatalog authenticates the release-owned runtime catalog installed
// in the worker image. The paths are intentionally fixed internal packaging
// inputs rather than operator-configurable settings.
func LoadRuntimeCatalog() (*RuntimeCatalog, error) {
	catalogBytes, err := readRuntimeReleaseFileOwned(
		runtimeCatalogPath,
		"runtime catalog",
		maxRuntimeCatalogBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	bundleBytes, err := readRuntimeReleaseFileOwned(
		runtimeBundlePath,
		"runtime attestation bundle",
		maxReleaseBundleBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	trustedRootBytes, err := readRuntimeReleaseFileOwned(
		runtimeTrustedRootPath,
		"runtime trusted root",
		maxReleaseTrustedRootBytes,
		0,
	)
	if err != nil {
		return nil, err
	}
	return VerifyRuntimeCatalog(catalogBytes, bundleBytes, trustedRootBytes)
}

func readRuntimeReleaseFile(path, label string, maxBytes int64) ([]byte, error) {
	return readRuntimeReleaseFileOwned(path, label, maxBytes, uint32(os.Getuid()))
}

func readRuntimeReleaseFileOwned(
	path,
	label string,
	maxBytes int64,
	ownerUID uint32,
) ([]byte, error) {
	file, err := openRuntimeReleaseFile(path, label, maxBytes, ownerUID)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", label, err)
	}
	if len(raw) == 0 || int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("%s size is outside [1,%d]", label, maxBytes)
	}
	return raw, nil
}
