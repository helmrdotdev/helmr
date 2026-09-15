package buildcontext

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureUsesOnlyHelmrIgnoreAndRootGitRule(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".helmrignore"), "node_modules/\nignored/**\n!ignored/keep.ts\n.env\n")
	writeTestFile(t, filepath.Join(root, ".git", "config"), "git")
	writeTestFile(t, filepath.Join(root, "node_modules", "pkg", "index.js"), "dependency")
	writeTestFile(t, filepath.Join(root, "ignored", "drop.ts"), "drop")
	writeTestFile(t, filepath.Join(root, "ignored", "keep.ts"), "keep")
	writeTestFile(t, filepath.Join(root, "tasks", "task.test.ts"), "test")
	writeTestFile(t, filepath.Join(root, ".env"), "secret")

	result, err := Capture(t.Context(), root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	names := readCapturedNames(t, result.Path)
	for _, name := range []string{".git", ".git/config", "node_modules", "node_modules/pkg/index.js", "ignored/drop.ts"} {
		if names[name] {
			t.Fatalf("build source contains excluded %q: %+v", name, names)
		}
	}
	for _, name := range []string{".helmrignore", "ignored/keep.ts", "tasks/task.test.ts"} {
		if !names[name] {
			t.Fatalf("build source omits %q: %+v", name, names)
		}
	}
}

func TestCaptureRejectsRetainedEnvironmentSecrets(t *testing.T) {
	for _, name := range []string{".env", ".env.local", "packages/api/.env.production"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, filepath.FromSlash(name)), "TOKEN=secret")
			if _, err := Capture(t.Context(), root, t.TempDir()); err == nil || !strings.Contains(err.Error(), "likely secret") {
				t.Fatalf("build source error = %v", err)
			}
		})
	}
}

func TestCaptureAcceptsEnvironmentExamples(t *testing.T) {
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
	result, err := Capture(t.Context(), root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	names := readCapturedNames(t, result.Path)
	for _, name := range []string{
		".env.example",
		".env.production.example",
		".env.production.sample",
		"packages/api/.env.template",
	} {
		if !names[name] {
			t.Fatalf("build source omits environment example %q", name)
		}
	}
}

func TestCaptureRejectsUnignoredAuthorityRoots(t *testing.T) {
	for _, name := range []string{"node_modules", "helmr"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, name, "entry"), "x")
			if _, err := Capture(t.Context(), root, t.TempDir()); err == nil {
				t.Fatalf("build source accepted root %q", name)
			}
		})
	}
}

func TestCaptureAllowsIgnoredAndNestedAuthorityNames(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, ".helmrignore"), "/node_modules/\n/helmr/\n")
	writeTestFile(t, filepath.Join(root, "node_modules", "ignored"), "dependency")
	writeTestFile(t, filepath.Join(root, "helmr", "ignored"), "platform")
	writeTestFile(t, filepath.Join(root, "nested", "node_modules", "kept"), "nested")
	writeTestFile(t, filepath.Join(root, "nested", "helmr", "kept"), "nested")
	result, err := Capture(t.Context(), root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer result.Close()
	names := readCapturedNames(t, result.Path)
	for _, name := range []string{"node_modules", "helmr"} {
		if names[name] {
			t.Fatalf("build source contains ignored root %q", name)
		}
	}
	for _, name := range []string{"nested/node_modules/kept", "nested/helmr/kept"} {
		if !names[name] {
			t.Fatalf("build source omits nested path %q", name)
		}
	}
}

func TestCaptureExcludesEveryRootGitType(t *testing.T) {
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
			result, err := Capture(t.Context(), root, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			if names := readCapturedNames(t, result.Path); names[".git"] {
				t.Fatalf("build source contains root .git: %+v", names)
			}
		})
	}
}

func writeTestFile(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func readCapturedNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	if err := filepath.WalkDir(root, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, name)
		names[filepath.ToSlash(relative)] = true
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return names
}
