//go:build linux

package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckCommandRequiresExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand("tool", path); err == nil || !strings.Contains(err.Error(), "not executable") {
		t.Fatalf("error = %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkCommand("tool", path); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCNINetworkConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "helmr.conflist"), []byte(`{"cniVersion":"1.0.0","name":"helmr","plugins":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkCNINetworkConfig(dir, "helmr"); err != nil {
		t.Fatal(err)
	}
	if err := checkCNINetworkConfig(dir, "missing"); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestPreflightChecksContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := (&Connector{cfg: (Config{}).WithDefaults()}).Preflight(ctx)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("error = %v", err)
	}
}
