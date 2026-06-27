package archive

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestTarReadListAndStat(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src", "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "app.ts"), []byte("console.log('ok')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "nested", "mod.ts"), []byte("export {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("app.ts", filepath.Join(root, "src", "link.ts")); err != nil {
		t.Fatal(err)
	}
	tarball, cleanup, err := CreateTar(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.Open(tarball.Path)
	if err != nil {
		t.Fatal(err)
	}
	read, err := OpenTarEntry(body, "src/app.ts", ExtractOptions{MaxBytes: 1024, MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(read.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "console.log('ok')\n" {
		t.Fatalf("read contents = %q", string(contents))
	}
	if read.Entry.Path != "src/app.ts" || read.Entry.Kind != TarEntryKindFile {
		t.Fatalf("entry = %+v", read.Entry)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	body, err = os.Open(tarball.Path)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ListTarEntries(body, "src", ExtractOptions{MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	slices.Sort(paths)
	wantPaths := []string{"src/app.ts", "src/link.ts", "src/nested"}
	if !slices.Equal(paths, wantPaths) {
		t.Fatalf("list paths = %v, want %v", paths, wantPaths)
	}
	var nested TarEntry
	for _, entry := range entries {
		if entry.Path == "src/nested" {
			nested = entry
			break
		}
	}
	if nested.Mode == 0 || nested.ModTime.IsZero() {
		t.Fatalf("listed nested directory lost metadata: %+v", nested)
	}

	body, err = os.Open(tarball.Path)
	if err != nil {
		t.Fatal(err)
	}
	stat, err := StatTarEntry(body, "src/nested", ExtractOptions{MaxEntries: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if stat.Path != "src/nested" || stat.Kind != TarEntryKindDir {
		t.Fatalf("stat = %+v", stat)
	}
}

func TestOpenTarEntryRejectsNonFileAndReadCap(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "big.txt"), []byte("too large"), 0o644); err != nil {
		t.Fatal(err)
	}
	tarball, cleanup, err := CreateTar(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	body, err := os.Open(tarball.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTarEntry(body, "src", ExtractOptions{MaxBytes: 1024, MaxEntries: 100}); !errors.Is(err, ErrTarEntryNotFile) {
		t.Fatalf("OpenTarEntry dir err = %v, want %v", err, ErrTarEntryNotFile)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	body, err = os.Open(tarball.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenTarEntry(body, "src/big.txt", ExtractOptions{MaxBytes: 3, MaxEntries: 100}); !errors.Is(err, ErrTarEntryTooLarge) {
		t.Fatalf("OpenTarEntry cap err = %v, want %v", err, ErrTarEntryTooLarge)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
}
