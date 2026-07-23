package projectconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureExtractsInspector(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	inspector, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if inspector.Dir == "" || inspector.ScriptPath == "" || inspector.RegisterPath == "" {
		t.Fatalf("inspector paths not populated: %+v", inspector)
	}
	for _, path := range []string{
		inspector.ScriptPath,
		inspector.RegisterPath,
		filepath.Join(inspector.Dir, "loader.mjs"),
		filepath.Join(inspector.Dir, "manifest.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s is not a regular file", path)
		}
	}
}

func TestEnsureRepairsCorruptCache(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	inspector, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inspector.ScriptPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	repaired, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Dir != inspector.Dir {
		t.Fatalf("cache dir changed after repair: %s != %s", repaired.Dir, inspector.Dir)
	}
	body, err := os.ReadFile(repaired.ScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "corrupt" {
		t.Fatal("corrupt inspector cache was not repaired")
	}
}

func TestEnsureUsesExplicitCacheDir(t *testing.T) {
	cacheRoot := t.TempDir()
	if err := os.Chmod(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_CONFIG_CACHE_DIR", cacheRoot)

	inspector, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if wantPrefix := filepath.Join(cacheRoot, "config"); !hasPathPrefix(inspector.Dir, wantPrefix) {
		t.Fatalf("config dir %q is not under %q", inspector.Dir, wantPrefix)
	}
	info, err := os.Stat(filepath.Join(cacheRoot, "config"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("explicit config cache permissions are too open: %o", info.Mode().Perm())
	}
}

func TestEnsureFallsBackToPrivateTempCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HELMR_CONFIG_CACHE_DIR", "")

	inspector, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if again.Dir != inspector.Dir {
		t.Fatalf("fallback cache dir changed across Ensure calls: %s != %s", again.Dir, inspector.Dir)
	}
	if !hasPathPrefix(inspector.Dir, tmp) {
		t.Fatalf("config dir %q is not under temp dir %q", inspector.Dir, tmp)
	}
	privateRoot := filepath.Dir(filepath.Dir(inspector.Dir))
	info, err := os.Stat(privateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("fallback cache parent permissions are too open: %o", info.Mode().Perm())
	}
}

func hasPathPrefix(path string, prefix string) bool {
	rel, err := filepath.Rel(prefix, path)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel)
}
