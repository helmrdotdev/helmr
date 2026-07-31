package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRequiresWorkerGroupsInProviderRegion(t *testing.T) {
	setDevRegionConfig(t, "local")
	if _, err := loadConfig(); err != nil {
		t.Fatalf("matching Region mapping: %v", err)
	}

	setDevRegionConfig(t, "elsewhere")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), `worker group "local-workers" region "elsewhere" must match HELMR_PROVIDER_REGION "local"`) {
		t.Fatalf("mismatched Region mapping error = %v", err)
	}
}

func setDevRegionConfig(t *testing.T, workerGroupRegion string) {
	t.Helper()
	t.Setenv("HELMR_DATABASE_URL", "postgres://example")
	t.Setenv("HELMR_BUILD_POLICY_PATH", "/etc/helmr/build-policy.json")
	t.Setenv("HELMR_REGION_ID", "local")
	t.Setenv("HELMR_DEFAULT_REGION_ID", "local")
	t.Setenv("HELMR_PROVIDER", "local")
	t.Setenv("HELMR_PROVIDER_REGION", "local")
	t.Setenv("HELMR_WORKER_GROUP_ID", "local-workers")
	t.Setenv("HELMR_WORKER_GROUPS", `[{"id":"local-workers","name":"local","allows_run":true,"allows_build":true,"region":"`+workerGroupRegion+`","account_id":"000000000000","autoscaling_group":"helmr-local","instance_profile_arn":"arn:aws:iam::000000000000:instance-profile/helmr-local","launch_ami_id":"ami-local","ami_ids":["ami-local"]}]`)
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
