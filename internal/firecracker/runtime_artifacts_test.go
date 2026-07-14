package firecracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestLoadRuntimeArtifacts(t *testing.T) {
	cfg, manifest := writeRuntimeArtifactFixture(t)
	artifacts, err := loadRuntimeArtifacts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if artifacts != manifest {
		t.Fatalf("artifacts = %+v, want %+v", artifacts, manifest)
	}
}

func TestLoadRuntimeArtifactsRejectsInvalidManifest(t *testing.T) {
	tests := []struct {
		name string
		edit func(Config, *runtimeArtifacts)
		raw  string
		want string
	}{
		{name: "missing", edit: func(cfg Config, _ *runtimeArtifacts) { _ = os.Remove(cfg.RuntimeArtifactsPath) }, want: "open runtime artifacts manifest"},
		{name: "malformed", raw: "{", want: "decode runtime artifacts manifest"},
		{name: "trailing", raw: "{}{}", want: "runtime artifacts manifest contains trailing JSON"},
		{name: "unknown field", raw: `{"unknown":true}`, want: "unknown field"},
		{name: "schema", edit: func(_ Config, m *runtimeArtifacts) { m.Schema = "other" }, want: "schema"},
		{name: "arch", edit: func(_ Config, m *runtimeArtifacts) { m.Arch = "other" }, want: "arch"},
		{name: "abi", edit: func(_ Config, m *runtimeArtifacts) { m.RuntimeABI = "other" }, want: "abi"},
		{name: "path", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Path = "other" }, want: "rootfs path"},
		{name: "digest", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Digest = "sha256:not-a-digest" }, want: "canonical sha256"},
		{name: "uppercase digest", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Digest = "sha256:" + strings.Repeat("A", 64) }, want: "canonical sha256"},
		{name: "size", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.SizeBytes++ }, want: "does not match manifest size"},
		{name: "missing artifact", edit: func(cfg Config, _ *runtimeArtifacts) { _ = os.Remove(cfg.RootfsPath) }, want: "stat runtime artifacts rootfs"},
		{name: "symlink artifact", edit: func(cfg Config, _ *runtimeArtifacts) {
			target := cfg.RootfsPath + ".target"
			if err := os.Rename(cfg.RootfsPath, target); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, cfg.RootfsPath); err != nil {
				t.Fatal(err)
			}
		}, want: "is not a regular file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, manifest := writeRuntimeArtifactFixture(t)
			if tt.edit != nil {
				tt.edit(cfg, &manifest)
			}
			if tt.raw != "" {
				if err := os.WriteFile(cfg.RuntimeArtifactsPath, []byte(tt.raw), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if _, err := os.Stat(cfg.RuntimeArtifactsPath); err == nil {
				body, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg.RuntimeArtifactsPath, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := loadRuntimeArtifacts(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func writeRuntimeArtifactFixture(t *testing.T) (Config, runtimeArtifacts) {
	t.Helper()
	dir := t.TempDir()
	cfg := (Config{
		KernelPath:    filepath.Join(dir, "vmlinuz"),
		InitramfsPath: filepath.Join(dir, "initramfs"),
		RootfsPath:    filepath.Join(dir, "rootfs.ext4"),
	}).WithDefaults()
	write := func(path string, body string) runtimeArtifact {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return runtimeArtifact{Path: filepath.Base(path), Digest: sha256sum.DigestBytes([]byte(body)), SizeBytes: int64(len(body))}
	}
	manifest := runtimeArtifacts{
		Schema:     runtimeArtifactsSchema,
		Arch:       runtime.GOARCH,
		RuntimeABI: runtimeABI,
		Kernel:     write(cfg.KernelPath, "kernel"),
		Initramfs:  write(cfg.InitramfsPath, "initramfs"),
		Rootfs:     write(cfg.RootfsPath, "rootfs"),
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.RuntimeArtifactsPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, manifest
}
