package deployment

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestReadDeploymentBundleDirectory(t *testing.T) {
	directory, bundle := writeTestDeploymentBundleDirectory(t)
	loaded, err := ReadDeploymentBundleDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest == "" || loaded.Bundle.Contract != DeploymentBundleContract {
		t.Fatalf("loaded = %+v", loaded)
	}
	if len(loaded.Objects) != len(bundle.Objects) {
		t.Fatalf("objects = %+v", loaded.Objects)
	}
}

func TestReadDeploymentBundleDirectoryRejectsNonClosureFiles(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, string, DeploymentBundle)
		want   string
	}{
		{
			name: "extra root file",
			change: func(t *testing.T, directory string, _ DeploymentBundle) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "build.log"), []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "extra object",
			change: func(t *testing.T, directory string, _ DeploymentBundle) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "objects", "sha256", strings.Repeat("e", 64)), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "missing object",
			change: func(t *testing.T, directory string, bundle DeploymentBundle) {
				t.Helper()
				if err := os.Remove(filepath.Join(directory, "objects", "sha256", strings.TrimPrefix(bundle.Objects[0].Digest, "sha256:"))); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "symlink object",
			change: func(t *testing.T, directory string, bundle DeploymentBundle) {
				t.Helper()
				path := filepath.Join(directory, "objects", "sha256", strings.TrimPrefix(bundle.Objects[0].Digest, "sha256:"))
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("bundle.json", path); err != nil {
					t.Fatal(err)
				}
			},
			want: "not a regular file",
		},
		{
			name: "corrupt object",
			change: func(t *testing.T, directory string, bundle DeploymentBundle) {
				t.Helper()
				path := filepath.Join(directory, "objects", "sha256", strings.TrimPrefix(bundle.Objects[0].Digest, "sha256:"))
				if err := os.WriteFile(path, []byte(strings.Repeat("x", int(bundle.Objects[0].SizeBytes))), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "digest does not match",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory, bundle := writeTestDeploymentBundleDirectory(t)
			test.change(t, directory, bundle)
			if _, err := ReadDeploymentBundleDirectory(directory); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadDeploymentBundleDirectory error = %v, want %q", err, test.want)
			}
		})
	}
}

func writeTestDeploymentBundleDirectory(t *testing.T) (string, DeploymentBundle) {
	t.Helper()
	bundle := testDeploymentBundle(t)
	program := []byte("program")
	workspace := []byte("workspace")
	bundle.Program.Artifact.Digest = sha256sum.DigestBytes(program)
	bundle.Program.Artifact.SizeBytes = int64(len(program))
	bundle.WorkspaceImages[0].Artifact.Digest = sha256sum.DigestBytes(workspace)
	bundle.WorkspaceImages[0].Artifact.SizeBytes = int64(len(workspace))
	for index := range bundle.Program.Index.Declarations {
		if bundle.Program.Index.Declarations[index].Sandbox != nil {
			bundle.Program.Index.Declarations[index].Sandbox.Image.ArtifactDigest = bundle.WorkspaceImages[0].Artifact.Digest
		}
	}
	bundle.Objects = []BundleObject{
		{Digest: bundle.Program.Artifact.Digest, SizeBytes: int64(len(program)), MediaType: ProgramArtifactMediaType},
		{Digest: bundle.WorkspaceImages[0].Artifact.Digest, SizeBytes: int64(len(workspace)), MediaType: WorkspaceImageArtifactMediaType},
	}
	SortDeploymentBundleObjects(bundle.Objects)
	raw, err := CanonicalDeploymentBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	objects := filepath.Join(directory, "objects", "sha256")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "bundle.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{
		bundle.Program.Artifact.Digest:            program,
		bundle.WorkspaceImages[0].Artifact.Digest: workspace,
	}
	for digest, content := range contents {
		if err := os.WriteFile(filepath.Join(objects, strings.TrimPrefix(digest, "sha256:")), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory, bundle
}
