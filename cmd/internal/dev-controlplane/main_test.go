package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigAcceptsProviderNeutralWorkerGroup(t *testing.T) {
	setDevRegionConfig(t)
	if _, err := loadConfig(); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigRejectsWorkerGroupInfrastructureFields(t *testing.T) {
	setDevRegionConfig(t)
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"local-workers","region":"local"}]`)
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), `unknown field "region"`) {
		t.Fatalf("infrastructure field error = %v", err)
	}
}

func setDevRegionConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_BUILD_POLICY_PATH", "/etc/helmr/build-policy.json")
	t.Setenv("HELMR_REGION_ID", "local")
	t.Setenv("HELMR_DEFAULT_REGION_ID", "local")
	t.Setenv("HELMR_PROVIDER", "local")
	t.Setenv("HELMR_PROVIDER_REGION", "local")
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"local-workers","name":"local","enrollment_secret_env":"HELMR_WORKER_ENROLLMENT_SECRET_LOCAL","allows_run":true,"allows_build":true,"observation_ttl_seconds":120,"instance_capacity":{"milli_cpu":1000,"memory_bytes":1024,"guest_ephemeral_disk_bytes":2048,"vm_slots":1,"build_executors":1}}]`)
	t.Setenv("HELMR_WORKER_ENROLLMENT_SECRET_LOCAL", "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8")
	t.Setenv("HELMR_CLICKHOUSE_URL", "http://127.0.0.1:8123")
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
