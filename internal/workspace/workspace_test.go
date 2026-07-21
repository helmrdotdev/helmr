package workspace

import (
	"archive/tar"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateWorkspaceArtifactIncludesGitDirectory(t *testing.T) {
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte("[core]\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	artifact, cleanup, err := CreateWorkspaceArtifactFromRoot(root, t.TempDir(), filepath.Dir(root))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	file, err := os.Open(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == ".git/config" {
			return
		}
	}
	t.Fatal("workspace artifact omitted .git/config")
}
