package bundle

import (
	"github.com/helmrdotdev/helmr/internal/definition"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
)

func TestReadDeploymentBundleDirectory(t *testing.T) {
	directory, bundle := writeTestDeploymentBundleDirectory(t)
	loaded, err := ReadDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest == "" || loaded.Bundle.Contract != Contract {
		t.Fatalf("loaded = %+v", loaded)
	}
	if len(loaded.Objects) != len(bundle.Objects) {
		t.Fatalf("objects = %+v", loaded.Objects)
	}
}

func TestReadDeploymentBundleDirectoryRejectsNonClosureFiles(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, string, Manifest)
		want   string
	}{
		{
			name: "extra root file",
			change: func(t *testing.T, directory string, _ Manifest) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "build.log"), []byte("secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "extra object",
			change: func(t *testing.T, directory string, _ Manifest) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(directory, "objects", "sha256", strings.Repeat("e", 64)), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "missing object",
			change: func(t *testing.T, directory string, bundle Manifest) {
				t.Helper()
				if err := os.Remove(filepath.Join(directory, "objects", "sha256", strings.TrimPrefix(bundle.Objects[0].Digest, "sha256:"))); err != nil {
					t.Fatal(err)
				}
			},
			want: "entries do not match",
		},
		{
			name: "symlink object",
			change: func(t *testing.T, directory string, bundle Manifest) {
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
			change: func(t *testing.T, directory string, bundle Manifest) {
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
			if _, err := ReadDirectory(directory); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadDeploymentBundleDirectory error = %v, want %q", err, test.want)
			}
		})
	}
}

func writeTestDeploymentBundleDirectory(t *testing.T) (string, Manifest) {
	t.Helper()
	bundle := testDeploymentBundle(t)
	program := []byte("program")
	computer := []byte("computer")
	bundle.Program.Artifact.Digest = sha256sum.DigestBytes(program)
	bundle.Program.Artifact.SizeBytes = int64(len(program))
	bundle.ComputerSeeds[0].Artifact.Digest = sha256sum.DigestBytes(computer)
	bundle.ComputerSeeds[0].Artifact.SizeBytes = int64(len(computer))
	for index := range bundle.Program.Metadata.Definitions {
		if bundle.Program.Metadata.Definitions[index].Computer != nil {
			bundle.Program.Metadata.Definitions[index].Computer.Seed.ArtifactDigest = bundle.ComputerSeeds[0].Artifact.Digest
		}
	}
	bundle.Objects = []Object{
		{Digest: bundle.Program.Artifact.Digest, SizeBytes: int64(len(program)), MediaType: artifact.ProgramArtifactMediaType},
		{Digest: bundle.ComputerSeeds[0].Artifact.Digest, SizeBytes: int64(len(computer)), MediaType: definition.ComputerSeedMediaType},
	}
	SortObjects(bundle.Objects)
	raw, err := Canonical(bundle)
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
		bundle.Program.Artifact.Digest:          program,
		bundle.ComputerSeeds[0].Artifact.Digest: computer,
	}
	for digest, content := range contents {
		if err := os.WriteFile(filepath.Join(objects, strings.TrimPrefix(digest, "sha256:")), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory, bundle
}
