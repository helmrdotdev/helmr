package deployment

import "testing"

func TestToolchainReleasePathsAreFixed(t *testing.T) {
	if toolchainCatalogPath != "/usr/lib/helmr/toolchain-release/catalog.json" {
		t.Fatalf("standard-toolchain catalog path = %q", toolchainCatalogPath)
	}
	if toolchainBundlePath != "/usr/lib/helmr/toolchain-release/catalog.sigstore.json" {
		t.Fatalf("standard-toolchain bundle path = %q", toolchainBundlePath)
	}
	if toolchainTrustedRootPath != "/usr/lib/helmr/toolchain-release/trusted-root.json" {
		t.Fatalf("standard-toolchain trusted root path = %q", toolchainTrustedRootPath)
	}
}
