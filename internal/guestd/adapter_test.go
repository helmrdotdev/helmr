package guestd

import (
	"os"
	"path/filepath"
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
