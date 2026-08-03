package archive

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestExtractTarRestoresPermissionBitsDespiteUmask(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	for _, header := range []*tar.Header{
		{Name: "dir", Typeflag: tar.TypeDir, Mode: 0o777},
		{Name: "dir/file", Typeflag: tar.TypeReg, Mode: 0o666, Size: 1},
		{Name: "zero-dir", Typeflag: tar.TypeDir, Mode: 0},
		{Name: "zero", Typeflag: tar.TypeReg, Mode: 0, Size: 1},
	} {
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	previousUmask := syscall.Umask(0o077)
	defer syscall.Umask(previousUmask)
	destination := t.TempDir()
	if err := ExtractTar(bytes.NewReader(body.Bytes()), destination); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]os.FileMode{
		"dir":      0o777,
		"dir/file": 0o666,
		"zero-dir": 0,
		"zero":     0,
	} {
		info, err := os.Stat(filepath.Join(destination, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != expected {
			t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), expected)
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

func TestCanonicalSourceUsesOnlyHelmrIgnoreAndRootGitRule(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".helmrignore"), "node_modules/\nignored/**\n!ignored/keep.ts\n.env\n")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "git")
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "dependency")
	writeTestFile(t, filepath.Join(root, "ignored", "drop.ts"), "drop")
	writeTestFile(t, filepath.Join(root, "ignored", "keep.ts"), "keep")
	writeTestFile(t, filepath.Join(root, "tasks", "task.test.ts"), "test")
	writeTestFile(t, filepath.Join(root, ".env"), "secret")

	result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	names := readTarNames(t, result.Path)
	for _, name := range []string{".git", ".git/config", "node_modules", "node_modules/pkg/index.js", "ignored/drop.ts"} {
		if names[name] {
			t.Fatalf("canonical source contains excluded %q: %+v", name, names)
		}
	}
	for _, name := range []string{".helmrignore", "ignored/keep.ts", "tasks/task.test.ts"} {
		if !names[name] {
			t.Fatalf("canonical source omits %q: %+v", name, names)
		}
	}
}

func TestCanonicalSourceRejectsRetainedEnvironmentSecrets(t *testing.T) {
	for _, name := range []string{".env", ".env.local", "packages/api/.env.production"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), "TOKEN=secret")
			if _, _, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
				CanonicalSource: true,
			}); err == nil || !strings.Contains(err.Error(), "likely secret") {
				t.Fatalf("canonical source error = %v", err)
			}
		})
	}
}

func TestCanonicalSourceAcceptsEnvironmentExamples(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".helmrignore"), `.env
.env.*
!.env*.example
!.env*.sample
!.env*.template
`)
	for _, name := range []string{
		".env.example",
		".env.production.example",
		".env.production.sample",
		"packages/api/.env.template",
	} {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), "TOKEN=")
	}
	result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	names := readTarNames(t, result.Path)
	for _, name := range []string{
		".env.example",
		".env.production.example",
		".env.production.sample",
		"packages/api/.env.template",
	} {
		if !names[name] {
			t.Fatalf("canonical source omits environment example %q", name)
		}
	}
}

func TestCanonicalSourceRejectsUnignoredAuthorityRoots(t *testing.T) {
	for _, name := range []string{"node_modules", "helmr"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, name, "entry"), "x")
			if _, _, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
				CanonicalSource: true,
			}); err == nil {
				t.Fatalf("canonical source accepted root %q", name)
			}
		})
	}
}

func TestCanonicalSourceAllowsIgnoredAndNestedAuthorityNames(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".helmrignore"), "/node_modules/\n/helmr/\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "ignored"), "dependency")
	writeTestFile(t, filepath.Join(root, "helmr", "ignored"), "platform")
	writeTestFile(t, filepath.Join(root, "nested", "node_modules", "kept"), "nested")
	writeTestFile(t, filepath.Join(root, "nested", "helmr", "kept"), "nested")
	result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	names := readTarNames(t, result.Path)
	for _, name := range []string{"node_modules", "helmr"} {
		if names[name] {
			t.Fatalf("canonical source contains ignored root %q", name)
		}
	}
	for _, name := range []string{"nested/node_modules/kept", "nested/helmr/kept"} {
		if !names[name] {
			t.Fatalf("canonical source omits nested path %q", name)
		}
	}
}

func TestCanonicalSourceExcludesEveryRootGitType(t *testing.T) {
	tests := map[string]func(*testing.T, string){
		"file": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, ".git"), "gitdir: elsewhere")
		},
		"directory": func(t *testing.T, root string) {
			writeTestFile(t, filepath.Join(root, ".git", "config"), "git")
		},
		"symlink": func(t *testing.T, root string) {
			if err := os.Symlink("missing-gitdir", filepath.Join(root, ".git")); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			setup(t, root)
			writeTestFile(t, filepath.Join(root, "task.ts"), "task")
			result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
				CanonicalSource: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if names := readTarNames(t, result.Path); names[".git"] {
				t.Fatalf("canonical source contains root .git: %+v", names)
			}
		})
	}
}

func TestCanonicalSourcePreservesDanglingLinksAndHardLinksAsRegularFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "original"), "same bytes")
	if err := os.Link(filepath.Join(root, "original"), filepath.Join(root, "copy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
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
	types := make(map[string]byte)
	links := make(map[string]string)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types[header.Name] = header.Typeflag
		links[header.Name] = header.Linkname
	}
	if types["original"] != tar.TypeReg || types["copy"] != tar.TypeReg {
		t.Fatalf("hard-linked source types = %+v, want independent regular files", types)
	}
	if types["dangling"] != tar.TypeSymlink || links["dangling"] != "missing" {
		t.Fatalf("dangling source link = type %d target %q", types["dangling"], links["dangling"])
	}
}

func TestCanonicalSourceRejectsTempDirInsideRoot(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "task.ts"), "task")
	if _, _, err := CreateTarWithOptions(root, filepath.Join(root, ".tmp"), TarOptions{
		CanonicalSource: true,
	}); err == nil {
		t.Fatal("canonical source accepted an archive temp dir inside its root")
	}
}

func TestCanonicalSourceRequiresFinalUSTARComponentEnvelope(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, strings.Repeat("a", 100), strings.Repeat("b", 55))
	writeTestFile(t, filepath.Join(parent, "c"), "content")
	if _, _, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
	}); err == nil {
		t.Fatal("canonical source accepted a path whose final-component parent exceeds 155 bytes")
	}
}

func TestCanonicalSourceEmitsDirectorySortKeysAndModes(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a", "child"), "child")
	writeTestFile(t, filepath.Join(root, "a."), "file")
	if err := os.Chmod(filepath.Join(root, "a", "child"), 0o711); err != nil {
		t.Fatal(err)
	}
	result, cleanup, err := CreateTarWithOptions(root, t.TempDir(), TarOptions{
		CanonicalSource: true,
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
	var names []string
	var modes []int64
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, header.Name)
		modes = append(modes, header.Mode)
	}
	if got := fmt.Sprint(names); got != "[a. a/ a/child]" {
		t.Fatalf("entry order = %s", got)
	}
	if got := fmt.Sprint(modes); got != "[420 493 493]" {
		t.Fatalf("entry modes = %s", got)
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
	for i := range 2 {
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
