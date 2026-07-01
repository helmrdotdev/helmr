package guestd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdapterDependencyInstallEnvUsesWritableCacheDirs(t *testing.T) {
	sourceRoot := t.TempDir()
	env, err := adapterDependencyInstallEnv(sourceRoot)
	if err != nil {
		t.Fatal(err)
	}

	wantHome := filepath.Join(sourceRoot, ".helmr-build", "home")
	wantCache := filepath.Join(sourceRoot, ".helmr-build", "cache")
	wantNpmCache := filepath.Join(wantCache, "npm")
	for _, dir := range []string{wantHome, wantCache, wantNpmCache} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s is not a directory", dir)
		}
	}
	if got := envValue(env, "HOME"); got != wantHome {
		t.Fatalf("HOME = %q, want %q", got, wantHome)
	}
	if got := envValue(env, "XDG_CACHE_HOME"); got != wantCache {
		t.Fatalf("XDG_CACHE_HOME = %q, want %q", got, wantCache)
	}
	if got := envValue(env, "npm_config_cache"); got != wantNpmCache {
		t.Fatalf("npm_config_cache = %q, want %q", got, wantNpmCache)
	}
	if got := envValue(env, "npm_config_update_notifier"); got != "false" {
		t.Fatalf("npm_config_update_notifier = %q, want false", got)
	}
}

func TestCheckpointStorageTelemetryReportsWorkspaceAndImageRootSizes(t *testing.T) {
	root := t.TempDir()
	tempRoot := filepath.Join(root, "tmp")
	imageRoot := filepath.Join(tempRoot, "helmr-run-1", "image")
	workspaceRoot := filepath.Join(imageRoot, "workspace")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imageRoot, "runtime.bin"), []byte("runtime"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "state.txt"), []byte("workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_GUESTD_TMPDIR", tempRoot)
	if err := os.MkdirAll(os.Getenv("HELMR_GUESTD_TMPDIR"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempRoot, "control.tmp"), []byte("temp"), 0o600); err != nil {
		t.Fatal(err)
	}

	telemetry := checkpointStorageTelemetry{
		RunID:        "run-1",
		RunWaitID:    "wait-1",
		CheckpointID: "checkpoint-1",
		ImageRoot:    collectPathUsage(imageRoot),
		Workspace:    collectPathUsage(workspaceRoot),
		GuestdTemp:   collectPathUsage(guestdTempRoot()),
	}
	telemetry.WorkspaceWithinImageRoot = pathWithinOrEqual(imageRoot, workspaceRoot)
	telemetry.ImageRootWithinGuestdTemp = pathWithinOrEqual(guestdTempRoot(), imageRoot)
	telemetry.WorkspaceWithinGuestdTemp = pathWithinOrEqual(guestdTempRoot(), workspaceRoot)
	telemetry.ImageRootExcludingWorkspaceApparentBytes = telemetry.ImageRoot.ApparentBytes - telemetry.Workspace.ApparentBytes
	telemetry.GuestdTempExcludingImageRootApparentBytes = telemetry.GuestdTemp.ApparentBytes - telemetry.ImageRoot.ApparentBytes
	payload, err := json.Marshal(telemetry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "state.txt") || strings.Contains(string(payload), "runtime.bin") {
		t.Fatalf("telemetry leaked file names: %s", payload)
	}
	if !telemetry.WorkspaceWithinImageRoot {
		t.Fatal("workspace should be reported under image root")
	}
	if !telemetry.ImageRootWithinGuestdTemp {
		t.Fatal("image root should be reported under guestd temp")
	}
	if !telemetry.WorkspaceWithinGuestdTemp {
		t.Fatal("workspace should be reported under guestd temp")
	}
	if telemetry.Workspace.Files != 1 || telemetry.Workspace.ApparentBytes == 0 {
		t.Fatalf("workspace usage = %+v, want one non-empty file", telemetry.Workspace)
	}
	if telemetry.ImageRoot.Files != 2 || telemetry.ImageRoot.ApparentBytes <= telemetry.Workspace.ApparentBytes {
		t.Fatalf("image root usage = %+v, workspace = %+v", telemetry.ImageRoot, telemetry.Workspace)
	}
	if telemetry.GuestdTemp.Files != 3 || telemetry.GuestdTempExcludingImageRootApparentBytes == 0 {
		t.Fatalf("guestd temp usage = %+v, excluding image root apparent = %d", telemetry.GuestdTemp, telemetry.GuestdTempExcludingImageRootApparentBytes)
	}
}
