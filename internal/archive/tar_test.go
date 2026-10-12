package archive

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCreateTarIsDeterministicAndKeepsCallerContent(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "src", "task.ts"), "task")
	if err := os.Symlink("task.ts", filepath.Join(root, "src", "task-link.ts")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".git", "config"), "git")
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "pkg")
	writeTestFile(t, filepath.Join(root, ".helmr", "cache"), "cache")
	writeTestFile(t, filepath.Join(root, ".next", "cache"), "next")

	first, cleanupFirst, err := CreateTarWithOptionsContext(t.Context(), root, t.TempDir(), TarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupFirst()

	if err := os.Chtimes(filepath.Join(root, "src", "task.ts"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, cleanupSecond, err := CreateTarWithOptionsContext(t.Context(), root, t.TempDir(), TarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupSecond()

	if first.Digest != second.Digest {
		t.Fatalf("digest changed after mtime update: %s != %s", first.Digest, second.Digest)
	}
	names := readTarNames(t, first.Path)
	if !names["src"] || !names["src/task.ts"] || !names["src/task-link.ts"] {
		t.Fatalf("source entries missing: %+v", names)
	}
	if !names[".git/config"] {
		t.Fatalf("caller content .git/config was not archived: %+v", names)
	}
	for _, name := range []string{"node_modules/pkg/index.js", ".helmr/cache", ".next/cache"} {
		if !names[name] {
			t.Fatalf("committed computer entry %q was not archived: %+v", name, names)
		}
	}
}

func TestNormalizeHeaderKeepsOnlyDurablePermissionBits(t *testing.T) {
	header := &tar.Header{Typeflag: tar.TypeReg, Mode: 0o6755}
	normalizeHeader(header, "tool", false)
	if header.Mode != 0o755 {
		t.Fatalf("regular file mode = %o, want 755", header.Mode)
	}

	header = &tar.Header{Typeflag: tar.TypeSymlink, Mode: 0}
	normalizeHeader(header, "link", false)
	if header.Mode != 0o777 {
		t.Fatalf("symlink mode = %o, want 777", header.Mode)
	}
}

func TestCanonicalMetadataNormalizesInstalledTreeWithoutSourceFiltering(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "node_modules", "@helmr", "index.js"), "module")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "git")
	if err := os.Chmod(filepath.Join(root, "node_modules", "@helmr"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "node_modules", "@helmr", "index.js"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, cleanup, err := CreateTarWithOptionsContext(t.Context(), root, t.TempDir(), TarOptions{
		CanonicalMetadata: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	file, err := os.Open(result.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	modes := map[string]int64{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		modes[header.Name] = header.Mode
	}
	for _, name := range []string{"node_modules/", "node_modules/@helmr/", "node_modules/@helmr/index.js", ".git/config"} {
		if _, ok := modes[name]; !ok {
			t.Fatalf("canonical metadata archive omitted %q: %+v", name, modes)
		}
	}
	if modes["node_modules/@helmr/"] != 0o755 {
		t.Fatalf("directory mode = %o, want 755", modes["node_modules/@helmr/"])
	}
	if modes["node_modules/@helmr/index.js"] != 0o644 {
		t.Fatalf("regular file mode = %o, want 644", modes["node_modules/@helmr/index.js"])
	}
}

func TestCreateTarWithOptionsExcludesGlobPatterns(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "agents", "task.ts"), "task")
	writeTestFile(t, filepath.Join(root, "agents", "task.test.ts"), "test")
	writeTestFile(t, filepath.Join(root, "secrets", "token.txt"), "secret")
	writeTestFile(t, filepath.Join(root, ".next", "cache"), "cache")

	archive, cleanup, err := CreateTarWithOptionsContext(t.Context(), root, t.TempDir(), TarOptions{
		ExcludePatterns: []string{"**/*.test.*", "secrets/**", "**/.next/**"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	names := readTarNames(t, archive.Path)
	if !names["agents/task.ts"] {
		t.Fatalf("source entry missing: %+v", names)
	}
	for _, name := range []string{"agents/task.test.ts", "secrets/token.txt", ".next/cache"} {
		if names[name] {
			t.Fatalf("excluded entry %q was archived: %+v", name, names)
		}
	}
}

func TestCreateTarAllowsEmptyRoot(t *testing.T) {
	archive, cleanup, err := CreateTarWithOptionsContext(t.Context(), t.TempDir(), t.TempDir(), TarOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	if archive.EntryCount != 0 {
		t.Fatalf("entry count = %d, want 0", archive.EntryCount)
	}
	if archive.SizeBytes <= 0 {
		t.Fatalf("size bytes = %d, want non-empty tar envelope", archive.SizeBytes)
	}
}

func readTarNames(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	names := map[string]bool{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names[header.Name] = true
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTarObserverFailureAndCancellationRemoveArchive(t *testing.T) {
	for _, mode := range []string{"header_error", "payload_error", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			root, temp := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "file"), []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			expected := errors.New("observer failed")
			observer := func(*tar.Header) (io.Writer, error) {
				switch mode {
				case "header_error":
					return nil, expected
				case "cancel":
					cancel()
					return io.Discard, nil
				default:
					return failingTarObserver{err: expected}, nil
				}
			}
			_, cleanup, err := CreateTarWithOptionsContext(ctx, root, temp, TarOptions{ObserveEntry: observer})
			cleanup()
			if mode == "cancel" {
				expected = context.Canceled
			}
			if !errors.Is(err, expected) {
				t.Fatalf("error=%v want %v", err, expected)
			}
			entries, err := os.ReadDir(temp)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("leaked files: %v", entries)
			}
		})
	}
}

type failingTarObserver struct{ err error }

func (w failingTarObserver) Write([]byte) (int, error) { return 0, w.err }
