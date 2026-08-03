package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/config"
)

func TestFitsBuildHostComputeUsesDiskIndependentHostPool(t *testing.T) {
	if !fitsBuildHostCompute(compute.ResourceVector{
		MilliCPU:  3000,
		MemoryMiB: 4096,
		Slots:     1,
	}) {
		t.Fatal("fixed build envelope does not fit its exact host compute capacity")
	}
	if fitsBuildHostCompute(compute.ResourceVector{
		MilliCPU:  2999,
		MemoryMiB: 4096,
		Slots:     1,
	}) {
		t.Fatal("undersized host compute envelope was accepted")
	}
}

func TestCertifyBuildOnlyVMUsesCompleteImageBuildEnvelope(t *testing.T) {
	cfg := config.Worker{
		VMVCPUCount:      3,
		VMMemoryMiB:      4096,
		VMScratchDiskMiB: 32768,
	}
	resources, err := resolveVMResources(cfg, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if resources != compute.ImageBuildGuestResources() {
		t.Fatalf("resolved resources = %+v, want %+v", resources, compute.ImageBuildGuestResources())
	}

	cfg.VMVCPUCount = 2
	if _, err := resolveVMResources(cfg, false, true); err == nil ||
		!strings.Contains(err.Error(), "image-build guest") {
		t.Fatalf("undersized image-build VM error = %v", err)
	}
}

func TestValidateWorkerStoresRequiresDisjointNamespaces(t *testing.T) {
	base := config.Worker{
		CASURI:           "s3://ordinary",
		PlatformStoreURI: "s3://runtime/objects",
	}
	if err := validateWorkerStores(base); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*config.Worker){
		"ordinary runtime": func(cfg *config.Worker) {
			cfg.PlatformStoreURI = "s3://ordinary/runtime"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if err := validateWorkerStores(cfg); err == nil ||
				!strings.Contains(err.Error(), "distinct bucket authority") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestEnsureBuildCacheDirectoryRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "cache")
	if err := ensureBuildCacheDirectory(directory); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureBuildCacheDirectory(link); err == nil {
		t.Fatal("symlink cache directory was accepted")
	}
}

func TestEnsurePrivateDirectoryEnforcesPrivateDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "acquisition")
	if err := ensurePrivateDirectory(directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o, want 700", info.Mode().Perm())
	}

	public := filepath.Join(root, "public")
	if err := os.Mkdir(public, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(public); err == nil {
		t.Fatal("public directory was accepted")
	}

	link := filepath.Join(root, "link")
	if err := os.Symlink(directory, link); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(link); err == nil {
		t.Fatal("symlink was accepted")
	}
}

func TestRetryableWorkerCloserRetriesFailureAndMemoizesOnlySuccess(t *testing.T) {
	calls := 0
	closer := retryableWorkerCloser{close: func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("partial close")
		}
		return nil
	}}
	if err := closer.Close(context.Background()); err == nil {
		t.Fatal("first Close() unexpectedly succeeded")
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("retry Close() = %v", err)
	}
	if err := closer.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close() = %v", err)
	}
	if calls != 2 {
		t.Fatalf("underlying close calls = %d, want 2", calls)
	}
}
