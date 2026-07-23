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

func TestValidateWorkerStoresRequiresDisjointNamespaces(t *testing.T) {
	base := config.Worker{
		CASURI:          "s3://ordinary",
		RuntimeStoreURI: "s3://runtime/objects",
		ManagerStoreURI: "s3://managers",
	}
	if err := validateWorkerStores(base); err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*config.Worker){
		"ordinary runtime": func(cfg *config.Worker) {
			cfg.RuntimeStoreURI = "s3://ordinary/runtime"
		},
		"ordinary manager": func(cfg *config.Worker) {
			cfg.ManagerStoreURI = "s3://ordinary/managers"
		},
		"runtime manager": func(cfg *config.Worker) {
			cfg.ManagerStoreURI = "s3://runtime/managers"
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
