package builder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestInstalledLayoutContextBindsOneLinuxAMD64Manifest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "layout with spaces")
	digest := writeInstalledLayoutFixture(t, root, &oci.Platform{OS: "linux", Architecture: "amd64"})
	context, err := InstalledLayoutContext(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := "oci-layout://" + root + "@" + digest; context != want {
		t.Fatalf("context = %q, want %q", context, want)
	}
}

func TestInstalledLayoutContextRejectsWrongPlatformAndManifestBytes(t *testing.T) {
	t.Run("platform", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "layout")
		writeInstalledLayoutFixture(t, root, &oci.Platform{OS: "linux", Architecture: "arm64"})
		if _, err := InstalledLayoutContext(root); err == nil {
			t.Fatal("InstalledLayoutContext accepted an arm64 manifest")
		}
	})
	t.Run("manifest digest", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "layout")
		digest := writeInstalledLayoutFixture(t, root, &oci.Platform{OS: "linux", Architecture: "amd64"})
		path := filepath.Join(root, "blobs", "sha256", strings.TrimPrefix(digest, sha256sum.Prefix))
		if err := os.WriteFile(path, []byte("corrupt"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := InstalledLayoutContext(root); err == nil {
			t.Fatal("InstalledLayoutContext accepted corrupt manifest bytes")
		}
	})
}

func writeInstalledLayoutFixture(t *testing.T, root string, platform *oci.Platform) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := []byte(`{"schemaVersion":2,"config":{},"layers":[]}`)
	digest := sha256sum.DigestBytes(manifest)
	index, err := json.Marshal(oci.Index{Manifests: []oci.Descriptor{{
		MediaType: installedManifestMedia,
		Digest:    digest,
		Size:      int64(len(manifest)),
		Platform:  platform,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{
		filepath.Join(root, "oci-layout"): []byte(`{"imageLayoutVersion":"1.0.0"}`),
		filepath.Join(root, "index.json"): index,
		filepath.Join(root, "blobs", "sha256", strings.TrimPrefix(digest, sha256sum.Prefix)): manifest,
	} {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return digest
}
