package firecracker

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
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

func TestRuntimeCorpusBytesIncludesManifestAndFilesystemRounding(t *testing.T) {
	limit := BootCorpusMaxMiB * 1024 * 1024
	manifest := runtimeArtifacts{
		Kernel:    runtimeArtifact{SizeBytes: limit - 4*runtimeAllocationUnit},
		Initramfs: runtimeArtifact{SizeBytes: 1},
		Rootfs:    runtimeArtifact{SizeBytes: 1},
	}
	if _, err := runtimeCorpusBytes(manifest, 1); err != nil {
		t.Fatal(err)
	}
	manifest.Kernel.SizeBytes++
	if _, err := runtimeCorpusBytes(manifest, 1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("error = %v", err)
	}
	manifest.Kernel.SizeBytes = math.MaxInt64
	if _, err := runtimeCorpusBytes(manifest, 1); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("overflow error = %v", err)
	}
}

func TestMakeRuntimeArtifactsLoad(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve runtime artifact test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	cfg, want := writeRuntimeArtifactFixture(t)
	if err := os.Remove(cfg.RuntimeArtifactsPath); err != nil {
		t.Fatal(err)
	}
	// Supply existing small artifacts without invoking their image-build recipes.
	cmd := exec.Command("make", "--no-print-directory", "-s", "-f", filepath.Join(repoRoot, "images", "boot-artifacts.mk"),
		"--old-file="+cfg.KernelPath, "--old-file="+cfg.InitramfsPath, "--old-file="+cfg.RootfsPath,
		"REPO_ROOT="+repoRoot, "GUESTD_INPUT_HASH=fixture", "GUESTD_GO_IDENTITY=fixture",
		"KERNEL="+cfg.KernelPath, "INITRAMFS="+cfg.InitramfsPath, "ROOTFS="+cfg.RootfsPath,
		"RUNTIME_ARTIFACTS="+cfg.RuntimeArtifactsPath, cfg.RuntimeArtifactsPath)
	cmd.Dir = filepath.Join(repoRoot, "images", "guest")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("produce runtime manifest: %v\n%s", err, output)
	}
	artifacts, err := loadRuntimeArtifacts(cfg)
	// Boot images currently target amd64. Other workers must reject their output.
	if runtime.GOARCH != "amd64" {
		want := fmt.Sprintf("runtime artifacts arch %q does not match worker arch %q", "amd64", runtime.GOARCH)
		if err == nil || err.Error() != want {
			t.Fatalf("load Make runtime manifest: %v, want %q", err, want)
		}
		return
	}
	if err != nil {
		t.Fatalf("load Make runtime manifest: %v", err)
	}
	if artifacts != want {
		t.Fatalf("Make artifacts = %+v, want %+v", artifacts, want)
	}
}

func TestRuntimeArtifactContractCases(t *testing.T) {
	raw, err := os.ReadFile("../../tests/fixtures/runtime-artifacts/contracts.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name  string
		Value json.RawMessage
		Valid bool
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			cfg, manifest := writeRuntimeArtifactFixture(t)
			encoded, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			if len(tc.Value) == 0 {
				delete(fields, "vm_runtime_contract")
			} else {
				fields["vm_runtime_contract"] = tc.Value
			}
			encoded, err = json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg.RuntimeArtifactsPath, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			_, err = loadRuntimeArtifacts(cfg)
			if (err == nil) != tc.Valid {
				t.Fatalf("valid=%v: %v", tc.Valid, err)
			}
		})
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
		{name: "contract", edit: func(_ Config, m *runtimeArtifacts) { m.VMRuntimeContract = "other" }, want: "contract"},
		{name: "path", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Path = "other" }, want: "rootfs path"},
		{name: "digest", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Digest = "sha256:not-a-digest" }, want: "canonical sha256"},
		{name: "uppercase digest", edit: func(_ Config, m *runtimeArtifacts) { m.Rootfs.Digest = "sha256:" + strings.Repeat("A", 64) }, want: "canonical sha256"},
		{name: "content digest", edit: func(cfg Config, _ *runtimeArtifacts) {
			if err := os.WriteFile(cfg.RootfsPath, []byte("other!"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: "does not match manifest digest"},
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
		RootfsPath:    filepath.Join(dir, "rootfs.squashfs"),
	}).WithDefaults()
	write := func(path string, body string) runtimeArtifact {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return runtimeArtifact{Path: filepath.Base(path), Digest: sha256sum.DigestBytes([]byte(body)), SizeBytes: int64(len(body))}
	}
	manifest := runtimeArtifacts{
		Schema:            runtimeArtifactsSchema,
		Arch:              runtime.GOARCH,
		VMRuntimeContract: vmplatform.Contract,
		Kernel:            write(cfg.KernelPath, "kernel"),
		Initramfs:         write(cfg.InitramfsPath, "initramfs"),
		Rootfs:            write(cfg.RootfsPath, "rootfs"),
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
