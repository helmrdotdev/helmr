package deployment

import "testing"

func TestToolRegistryReleasePathsAreFixed(t *testing.T) {
	if toolRegistryPath != "/usr/lib/helmr/runtime-release/tool-registry.json" {
		t.Fatalf("dependency tool registry path = %q", toolRegistryPath)
	}
	if toolBundlePath != "/usr/lib/helmr/runtime-release/tool-registry.sigstore.json" {
		t.Fatalf("dependency tool bundle path = %q", toolBundlePath)
	}
}
