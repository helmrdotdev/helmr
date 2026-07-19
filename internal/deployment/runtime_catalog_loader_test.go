package deployment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeReleasePathsAreFixed(t *testing.T) {
	if runtimeCatalogPath != "/usr/lib/helmr/runtime-release/catalog.json" {
		t.Fatalf("runtime catalog path = %q", runtimeCatalogPath)
	}
	if runtimeBundlePath != "/usr/lib/helmr/runtime-release/catalog.sigstore.json" {
		t.Fatalf("runtime bundle path = %q", runtimeBundlePath)
	}
	if runtimeTrustedRootPath != "/usr/lib/helmr/runtime-release/trusted-root.json" {
		t.Fatalf("runtime trusted root path = %q", runtimeTrustedRootPath)
	}
}

func TestReadRuntimeReleaseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release.json")
	if err := os.WriteFile(path, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := readReleaseFile(path, "test release file", 7)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "release" {
		t.Fatalf("release file = %q", raw)
	}
}

func TestReadRuntimeReleaseFileFailsClosed(t *testing.T) {
	directory := t.TempDir()
	tests := []struct {
		name    string
		content []byte
		max     int64
		want    string
	}{
		{name: "empty", max: 1, want: "outside [1,1]"},
		{name: "oversized", content: []byte("ab"), max: 1, want: "outside [1,1]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(directory, test.name)
			if err := os.WriteFile(path, test.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readReleaseFile(path, "test release file", test.max); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("readReleaseFile error = %v, want %q", err, test.want)
			}
		})
	}

	if _, err := readReleaseFile(
		filepath.Join(directory, "missing"),
		"test release file",
		1,
	); err == nil || !strings.Contains(err.Error(), "open test release file") {
		t.Fatalf("missing release file error = %v", err)
	}
}
