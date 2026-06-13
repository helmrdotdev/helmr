package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
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

	first, cleanupFirst, err := CreateTar(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanupFirst()

	if err := os.Chtimes(filepath.Join(root, "src", "task.ts"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	second, cleanupSecond, err := CreateTar(root, t.TempDir())
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
			t.Fatalf("committed workspace entry %q was not archived: %+v", name, names)
		}
	}
}

func TestCreateTarWithOptionsExcludesGlobPatterns(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "tasks", "task.ts"), "task")
	writeTestFile(t, filepath.Join(root, "tasks", "task.test.ts"), "test")
	writeTestFile(t, filepath.Join(root, "secrets", "token.txt"), "secret")
	writeTestFile(t, filepath.Join(root, ".next", "cache"), "cache")

	archive, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		ExcludePatterns: []string{"**/*.test.*", "secrets/**", "**/.next/**"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	names := readTarNames(t, archive.Path)
	if !names["tasks/task.ts"] {
		t.Fatalf("source entry missing: %+v", names)
	}
	for _, name := range []string{"tasks/task.test.ts", "secrets/token.txt", ".next/cache"} {
		if names[name] {
			t.Fatalf("excluded entry %q was archived: %+v", name, names)
		}
	}
}

func TestCreateTarAllowsEmptyRoot(t *testing.T) {
	archive, cleanup, err := CreateTar(t.TempDir(), t.TempDir())
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

func TestExtractTarWithStatsCountsEntries(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "dir/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	stats, err := ExtractTarWithStats(bytes.NewReader(body.Bytes()), t.TempDir(), ExtractOptions{
		MaxBytes:   defaultMaxExtractedBytes,
		MaxEntries: defaultMaxExtractedEntries,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.EntryCount != 2 {
		t.Fatalf("entry count = %d, want 2", stats.EntryCount)
	}
	if stats.SizeBytes != 1 {
		t.Fatalf("size bytes = %d, want 1", stats.SizeBytes)
	}
}

func TestExtractTarRejectsUnsafePaths(t *testing.T) {
	for _, name := range []string{"../escape.txt", "/escape.txt"} {
		t.Run(name, func(t *testing.T) {
			var body bytes.Buffer
			writer := tar.NewWriter(&body)
			if err := writer.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ExtractTar(bytes.NewReader(body.Bytes()), t.TempDir()); err == nil {
				t.Fatal("expected unsafe archive path to be rejected")
			}
		})
	}
}

func TestExtractTarPreservesSafeSymlinks(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "target.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "target.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "dir/nested-link", Typeflag: tar.TypeSymlink, Linkname: "../target.txt"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: "dir/root-link", Typeflag: tar.TypeSymlink, Linkname: ".."}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := ExtractTar(bytes.NewReader(body.Bytes()), dest); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(filepath.Join(dest, "link"))
	if err != nil {
		t.Fatal(err)
	}
	if link != "target.txt" {
		t.Fatalf("link target = %q", link)
	}
	nested, err := os.Readlink(filepath.Join(dest, "dir", "nested-link"))
	if err != nil {
		t.Fatal(err)
	}
	if nested != "../target.txt" {
		t.Fatalf("nested link target = %q", nested)
	}
	rootLink, err := os.Readlink(filepath.Join(dest, "dir", "root-link"))
	if err != nil {
		t.Fatal(err)
	}
	if rootLink != ".." {
		t.Fatalf("root link target = %q", rootLink)
	}
}

func TestExtractTarRejectsUnsafeSymlinks(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside"}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(bytes.NewReader(body.Bytes()), t.TempDir()); err == nil {
		t.Fatal("expected symlink archive entry to be rejected")
	}
}

func TestExtractTarRejectsSymlinkParent(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "link/file.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(destination, "link")); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTar(bytes.NewReader(body.Bytes()), destination); err == nil {
		t.Fatal("expected symlink parent to be rejected")
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt")); !os.IsNotExist(err) {
		t.Fatalf("file escaped through symlink parent, stat err = %v", err)
	}
}

func TestExtractTarRejectsOversizedRegularFile(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	if err := writer.WriteHeader(&tar.Header{Name: "large.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("xx")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarWithOptions(bytes.NewReader(body.Bytes()), t.TempDir(), ExtractOptions{
		MaxBytes:   1,
		MaxEntries: defaultMaxExtractedEntries,
	}); err == nil {
		t.Fatal("expected oversized archive entry to be rejected")
	}
}

func TestExtractTarRejectsTooManyEntries(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for i := 0; i < 2; i++ {
		if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("dirs/entry-%d", i), Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractTarWithOptions(bytes.NewReader(body.Bytes()), t.TempDir(), ExtractOptions{
		MaxBytes:   defaultMaxExtractedBytes,
		MaxEntries: 1,
	}); err == nil {
		t.Fatal("expected archive with too many entries to be rejected")
	}
}

func TestExtractTarRejectsSparseMetadata(t *testing.T) {
	var extracted int64
	if err := validateHeaderSize(&tar.Header{
		Name:     "sparse.bin",
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     1,
		PAXRecords: map[string]string{
			"GNU.sparse.realsize": "1099511627776",
		},
	}, &extracted, defaultMaxExtractedBytes); err == nil {
		t.Fatal("expected sparse archive entry to be rejected")
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
