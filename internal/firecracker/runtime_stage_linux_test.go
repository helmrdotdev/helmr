//go:build linux

package firecracker

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPrepareRuntimePublishesVerifiedCorpus(t *testing.T) {
	cfg, manifest := writeRuntimeArtifactFixture(t)
	workDir := t.TempDir()
	epoch := uuid.Must(uuid.NewV7()).String()

	stage, err := PrepareRuntime(filepath.Dir(cfg.RootfsPath), workDir, epoch)
	if err != nil {
		t.Fatal(err)
	}
	if stage != filepath.Join(workDir, runtimeStagePrefix+epoch) {
		t.Fatalf("stage = %q", stage)
	}
	got, err := loadRuntimeArtifacts(runtimeConfig(stage))
	if err != nil {
		t.Fatal(err)
	}
	if got != manifest {
		t.Fatalf("manifest = %+v, want %+v", got, manifest)
	}
	info, err := os.Stat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o500 {
		t.Fatalf("stage mode = %o, want 500", info.Mode().Perm())
	}
	for _, name := range []string{"vmlinuz", "initramfs", "rootfs.ext4", "runtime-artifacts.json"} {
		info, err := os.Stat(filepath.Join(stage, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o444 {
			t.Fatalf("%s mode = %o, want 444", name, info.Mode().Perm())
		}
	}
}

func TestPrepareRuntimeRejectsSymlink(t *testing.T) {
	cfg, _ := writeRuntimeArtifactFixture(t)
	target := cfg.KernelPath + ".target"
	if err := os.Rename(cfg.KernelPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, cfg.KernelPath); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRuntime(filepath.Dir(cfg.RootfsPath), t.TempDir(), uuid.Must(uuid.NewV7()).String())
	if err == nil || !strings.Contains(err.Error(), "open runtime artifacts kernel") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareRuntimeRejectsSymlinkedDirectory(t *testing.T) {
	cfg, _ := writeRuntimeArtifactFixture(t)
	link := filepath.Join(t.TempDir(), "source")
	if err := os.Symlink(filepath.Dir(cfg.RootfsPath), link); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRuntime(link, t.TempDir(), uuid.Must(uuid.NewV7()).String())
	if err == nil || !strings.Contains(err.Error(), "open runtime source directory") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareRuntimeRejectsDigestMismatch(t *testing.T) {
	cfg, _ := writeRuntimeArtifactFixture(t)
	if err := os.WriteFile(cfg.RootfsPath, []byte("other!"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := PrepareRuntime(filepath.Dir(cfg.RootfsPath), t.TempDir(), uuid.Must(uuid.NewV7()).String())
	if err == nil || !strings.Contains(err.Error(), "source digest does not match manifest") {
		t.Fatalf("error = %v", err)
	}
}

func TestCleanRuntimesRetainsCurrentEpoch(t *testing.T) {
	workDir := t.TempDir()
	keep := filepath.Join(workDir, runtimeStagePrefix+uuid.Must(uuid.NewV7()).String())
	stale := filepath.Join(workDir, runtimeStagePrefix+uuid.Must(uuid.NewV7()).String())
	partial := filepath.Join(workDir, "."+runtimeStagePrefix+"partial")
	for _, path := range []string{keep, stale, partial} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := CleanRuntimes(workDir, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("retained runtime: %v", err)
	}
	for _, path := range []string{stale, partial} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale runtime %q remains: %v", path, err)
		}
	}
}

func TestCleanRuntimesRejectsPrefixedFile(t *testing.T) {
	workDir := t.TempDir()
	path := filepath.Join(workDir, runtimeStagePrefix+"unexpected")
	if err := os.WriteFile(path, []byte("not a stage"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CleanRuntimes(workDir, "")
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("error = %v", err)
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

func TestCheckHardLinkLayout(t *testing.T) {
	root := t.TempDir()
	state := filepath.Join(root, "state")
	jailer := filepath.Join(root, "jailer")
	for _, path := range []string{state, jailer} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		KernelPath:          filepath.Join(root, "vmlinuz"),
		InitramfsPath:       filepath.Join(root, "initramfs"),
		RootfsPath:          filepath.Join(root, "rootfs.ext4"),
		StateDir:            state,
		JailerChrootBaseDir: jailer,
	}
	for _, path := range []string{cfg.KernelPath, cfg.InitramfsPath, cfg.RootfsPath} {
		if err := os.WriteFile(path, []byte("artifact"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := checkHardLinkLayout(cfg); err != nil {
		t.Fatal(err)
	}
}
