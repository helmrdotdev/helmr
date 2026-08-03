package workspace

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateWorkspaceArtifactHonorsCancelledContext(t *testing.T) {
	trustedRoot := t.TempDir()
	root := filepath.Join(trustedRoot, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, cleanup, err := CreateWorkspaceArtifactFromRootWithExcludesContext(
		ctx, root, t.TempDir(), trustedRoot, nil,
	)
	cleanup()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("create cancelled Workspace Artifact error = %v", err)
	}
}

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
