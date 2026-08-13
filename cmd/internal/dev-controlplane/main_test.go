package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsEmptyBootstrap(t *testing.T) {
	setDevRegionConfig(t)
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsInvalidDevBoolean(t *testing.T) {
	setDevRegionConfig(t)
	t.Setenv("HELMR_DEV_SEED_DATA", "yes")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "HELMR_DEV_SEED_DATA must be a boolean") {
		t.Fatalf("boolean error = %v", err)
	}
}

func TestDecodeRootKeyRejectsSurroundingWhitespace(t *testing.T) {
	if _, err := decodeRootKey("AUTH_KEY", " AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		t.Fatal("root key with surrounding whitespace was accepted")
	}
}

func TestLoadConfigRejectsSetupTokenWhitespace(t *testing.T) {
	setDevRegionConfig(t)
	t.Setenv("SETUP_TOKEN", " dev-setup-token")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "SETUP_TOKEN must not have surrounding whitespace") {
		t.Fatalf("setup token error = %v", err)
	}
}

func setDevRegionConfig(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("DEPLOYMENT_RUNTIME_DESCRIPTOR_PATH", "/etc/helmr/runtime.descriptor.json")
	t.Setenv("CLICKHOUSE_URL", "http://127.0.0.1:8123")
}

func TestMigrationPathsFindsSourceRootWhenCwdDiffers(t *testing.T) {
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previous); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}

	paths, err := migrationPaths()
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("expected migration paths")
	}
	for _, path := range paths {
		if filepath.IsAbs(path) {
			continue
		}
		t.Fatalf("expected fallback migration path to be absolute, got %q", path)
	}
}
