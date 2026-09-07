package workspace

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
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

func TestCreateWorkspaceArtifactComputesEmittedTree(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "dir", "file"), []byte("hello"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir/file", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "secret"), []byte("excluded"), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, captured, cleanup, err := CaptureWorkspaceArtifactContext(t.Context(), root, t.TempDir(), root, []string{"secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	tree, err := InspectArtifactTreeContext(t.Context(), artifact.Path, artifact.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if captured != tree || tree.EntryCount != 3 || tree.SizeBytes != 5 {
		t.Fatalf("created tree=%+v inspected=%+v", captured, tree)
	}
}

func TestCreateWorkspaceArtifactRejectsUnsafeTreeAndCleansUp(t *testing.T) {
	root, temp := t.TempDir(), t.TempDir()
	if err := os.Symlink("../outside", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	_, _, cleanup, err := CaptureWorkspaceArtifactContext(t.Context(), root, temp, root, nil)
	cleanup()
	if err == nil {
		t.Fatal("invalid tree accepted")
	}
	files, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("leaked archive files: %v", files)
	}
}

func TestCreateWorkspaceArtifactOrdersDirectoryPrefixes(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a.txt", "a/file"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	artifact, captured, cleanup, err := CaptureWorkspaceArtifactContext(t.Context(), root, t.TempDir(), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	tree, err := InspectArtifactTreeContext(t.Context(), artifact.Path, artifact.SizeBytes)
	if err != nil {
		t.Fatal(err)
	}
	if captured != tree {
		t.Fatalf("emitted tree=%+v filesystem tree=%+v", captured, tree)
	}
}

func TestCaptureWorkspaceParity(t *testing.T) {
	for _, size := range []int{-1, 0, 1, 511, 512, 513, 65537} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			root := t.TempDir()
			if size >= 0 {
				if err := os.WriteFile(filepath.Join(root, "data"), bytes.Repeat([]byte("x"), size), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			artifact, tree, cleanup, err := CaptureWorkspaceArtifactContext(t.Context(), root, t.TempDir(), root, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			actual, err := InspectTree(root)
			if err != nil {
				t.Fatal(err)
			}
			if tree != actual {
				t.Fatalf("capture=%+v filesystem=%+v", tree, actual)
			}
			file, err := os.Open(artifact.Path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if err := VerifyArtifact(file, artifact, tree); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCaptureWorkspaceCancellationCleansUp(t *testing.T) {
	root, temp := t.TempDir(), t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	artifact, tree, cleanup, err := CaptureWorkspaceArtifactContext(ctx, root, temp, root, nil)
	cleanup()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if artifact != (WorkspaceArtifact{}) || tree != (TreeIdentity{}) {
		t.Fatalf("cancelled capture returned result: %+v %+v", artifact, tree)
	}
	entries, err := os.ReadDir(temp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("leaked files: %v", entries)
	}
}
