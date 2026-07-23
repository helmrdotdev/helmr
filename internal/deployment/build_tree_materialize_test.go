package deployment

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeApplicationExcludesOnlyRootManagerNamespace(t *testing.T) {
	artifact := newMemoryArtifact()
	artifact.addDirectory("node_modules")
	artifact.addDirectory("node_modules/root-package")
	artifact.addFile("node_modules/root-package/index.js", []byte("root"), 0o644)
	artifact.addDirectory("packages")
	artifact.addDirectory("packages/app")
	artifact.addDirectory("packages/app/node_modules")
	artifact.addDirectory("packages/app/node_modules/nested-package")
	artifact.addFile(
		"packages/app/node_modules/nested-package/index.js",
		[]byte("nested"),
		0o644,
	)
	artifact.addDirectory("packages/app/helmr")
	artifact.addFile("packages/app/helmr/value.txt", []byte("nested helmr"), 0o644)
	inspected, err := inspectMemoryBuildTree(t, artifact)
	if err != nil {
		t.Fatal(err)
	}
	tree := &BuildTree{
		content:   &artifactSnapshot{},
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
