package deployment

import (
	"strings"
	"testing"
)

func TestManagerCatalogCanonicalRoundTrip(t *testing.T) {
	bun := testManager(PackageManagerBun, ArchitectureX8664)
	npm := testManager(PackageManagerNPM, ArchitectureX8664)
	raw, err := CanonicalManagerCatalog([]Manager{bun, npm})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := ParseManagerCatalog(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Resolve(bun.PackageManager, bun.Architecture); err == nil ||
		!strings.Contains(err.Error(), "authenticated") {
		t.Fatalf("unauthenticated Resolve error = %v", err)
	}
	catalog.authenticated = true
	resolved, err := catalog.ResolvePinned(
		npm.PackageManager,
		npm.Architecture,
		npm.Tree.Digest,
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != npm {
		t.Fatalf("resolved Manager = %#v, want %#v", resolved, npm)
	}
	if _, err := catalog.ResolvePinned(
		npm.PackageManager,
		npm.Architecture,
		"sha256:"+strings.Repeat("f", 64),
	); !strings.Contains(err.Error(), "certified bytes") {
		t.Fatalf("wrong digest error = %v", err)
	}
}

func TestManagerCatalogRejectsUncertifiedAndNoncanonicalEntries(t *testing.T) {
	bun := testManager(PackageManagerBun, ArchitectureX8664)
	npm := testManager(PackageManagerNPM, ArchitectureX8664)
	if _, err := CanonicalManagerCatalog([]Manager{npm, bun}); err == nil ||
		!strings.Contains(err.Error(), "order") {
		t.Fatalf("out-of-order error = %v", err)
	}
	bun.Architecture = RuntimeArchitecture("aarch64")
	if _, err := CanonicalManagerCatalog([]Manager{bun}); err == nil ||
		!strings.Contains(err.Error(), "x86_64") {
		t.Fatalf("architecture error = %v", err)
	}
}

func TestManagerCatalogLoaderUsesFixedReleasePaths(t *testing.T) {
	if managerCatalogPath != "/usr/lib/helmr/manager-release/catalog.json" ||
		managerBundlePath != "/usr/lib/helmr/manager-release/catalog.sigstore.json" ||
		managerTrustedRootPath != "/usr/lib/helmr/manager-release/trusted-root.json" {
		t.Fatal("Manager catalog loader paths are not fixed release inputs")
	}
	if managerObjectDirectory != "/usr/lib/helmr/manager-release/objects/sha256" {
		t.Fatal("Manager object directory is not a fixed release input")
	}
}
