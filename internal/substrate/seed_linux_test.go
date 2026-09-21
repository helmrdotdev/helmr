//go:build linux

package substrate

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/computer"
)

func TestBuildCapacitySizedSeed(t *testing.T) {
	mkfs, config := os.Getenv("HELMR_SUBSTRATE_MKFS_EXT4"), os.Getenv("HELMR_SUBSTRATE_MKE2FS_CONFIG")
	if mkfs == "" || config == "" {
		t.Skip("exact generator tools not configured")
	}
	dir := t.TempDir()
	var layer bytes.Buffer
	archive := tar.NewWriter(&layer)
	if err := archive.WriteHeader(&tar.Header{Name: "state", Mode: 0644, Size: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := archive.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(dir, "image.tar")
	if err := os.WriteFile(image, ociTarFromLayers(t, layer.Bytes()), 0600); err != nil {
		t.Fatal(err)
	}
	const capacity = int64(128 << 20)
	var first computer.SeedArtifact
	for iteration := range 2 {
		target := filepath.Join(t.TempDir(), "disk.filepack")
		seed, err := BuildSeed(t.Context(), image, target, dir, mkfs, config, capacity)
		if err != nil {
			t.Fatal(err)
		}
		if seed.Config.User != "agent" || seed.Config.WorkingDir != "/workspace" {
			t.Fatal("authored runtime config lost")
		}
		input, err := os.Open(target)
		if err != nil {
			t.Fatal(err)
		}
		err = computer.VerifySeed(t.Context(), input, seed.Artifact, capacity)
		input.Close()
		if err != nil {
			t.Fatal(err)
		}
		if iteration == 0 {
			first = seed.Artifact
		} else if seed.Artifact != first {
			t.Fatal("repeated build changed artifact")
		}
		if _, err := BuildSeed(t.Context(), image, target, dir, mkfs, config, capacity); err == nil {
			t.Fatal("existing artifact overwritten")
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "image.tar" {
		t.Fatal("raw or expanded build scratch retained")
	}
}
