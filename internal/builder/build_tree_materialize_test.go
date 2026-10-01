package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact/artifacttest"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
)

func TestMaterializeApplicationExcludesOnlyRootManagerNamespace(t *testing.T) {
	artifact := artifacttest.NewMemory()
	artifact.AddDirectory("node_modules")
	artifact.AddDirectory("node_modules/root-package")
	artifact.AddFile("node_modules/root-package/index.js", []byte("root"), 0o644)
	artifact.AddDirectory("packages")
	artifact.AddDirectory("packages/app")
	artifact.AddDirectory("packages/app/node_modules")
	artifact.AddDirectory("packages/app/node_modules/nested-package")
	artifact.AddFile(
		"packages/app/node_modules/nested-package/index.js",
		[]byte("nested"),
		0o644,
	)
	artifact.AddDirectory("packages/app/helmr")
	artifact.AddFile("packages/app/helmr/value.txt", []byte("nested helmr"), 0o644)
	inspected, err := inspectMemoryBuildTree(t, artifact)
	if err != nil {
		t.Fatal(err)
	}
	tree := &buildTree{
		content:   &snapshot.Artifact{},
		inspected: inspected,
	}
	root, cleanup, err := tree.MaterializeApplication(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanup(); err != nil {
			t.Fatal(err)
		}
	}()
	if _, err := os.Lstat(filepath.Join(root, "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("root node_modules exists: %v", err)
	}
	for _, name := range []string{
		"packages/app/node_modules/nested-package/index.js",
		"packages/app/helmr/value.txt",
	} {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("nested application path %q is missing: %v", name, err)
		}
	}
}
