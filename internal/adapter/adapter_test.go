package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureExtractsEmbeddedAdapter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	adapter, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Dir == "" || adapter.MainPath == "" || adapter.RegisterPath == "" {
		t.Fatalf("adapter paths not populated: %+v", adapter)
	}
	for _, path := range []string{
		adapter.MainPath,
		adapter.RegisterPath,
		filepath.Join(adapter.Dir, "loader.mjs"),
		filepath.Join(adapter.Dir, "manifest.json"),
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

	adapter, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(adapter.MainPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	repaired, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Dir != adapter.Dir {
		t.Fatalf("cache dir changed after repair: %s != %s", repaired.Dir, adapter.Dir)
	}
	body, err := os.ReadFile(repaired.MainPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "corrupt" {
		t.Fatal("corrupt adapter cache was not repaired")
	}
}

func TestEnsureUsesExplicitCacheDir(t *testing.T) {
	cacheRoot := t.TempDir()
	if err := os.Chmod(cacheRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HELMR_ADAPTER_CACHE_DIR", cacheRoot)

	adapter, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if wantPrefix := filepath.Join(cacheRoot, "adapter"); !hasPathPrefix(adapter.Dir, wantPrefix) {
		t.Fatalf("adapter dir %q is not under %q", adapter.Dir, wantPrefix)
	}
	info, err := os.Stat(filepath.Join(cacheRoot, "adapter"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("explicit adapter cache permissions are too open: %o", info.Mode().Perm())
	}
}

func TestEnsureFallsBackToPrivateTempCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HELMR_ADAPTER_CACHE_DIR", "")

	adapter, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	again, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if again.Dir != adapter.Dir {
		t.Fatalf("fallback cache dir changed across Ensure calls: %s != %s", again.Dir, adapter.Dir)
	}
	if !hasPathPrefix(adapter.Dir, tmp) {
		t.Fatalf("adapter dir %q is not under temp dir %q", adapter.Dir, tmp)
	}
	privateRoot := filepath.Dir(filepath.Dir(adapter.Dir))
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
