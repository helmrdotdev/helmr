package disk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func benchmarkComputer(b *testing.B) (string, string, ComputerArtifact) {
	b.Helper()
	root, temp := b.TempDir(), b.TempDir()
	if err := os.WriteFile(filepath.Join(root, "data"), bytes.Repeat([]byte("content!"), 1<<20), 0o600); err != nil {
		b.Fatal(err)
	}
	artifact, cleanup, err := CreateComputerArtifactFromRoot(root, temp, root)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(cleanup)
	return root, temp, artifact
}

func BenchmarkInspectArtifact(b *testing.B) {
	_, _, artifact := benchmarkComputer(b)
	body, err := os.ReadFile(artifact.Path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := InspectArtifact(bytes.NewReader(body), artifact); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCaptureComputer(b *testing.B) {
	root, temp, _ := benchmarkComputer(b)
	b.SetBytes(8 << 20)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, tree, cleanup, err := CaptureComputerArtifactContext(b.Context(), root, temp, root, nil)
		if err != nil {
			b.Fatal(err)
		}
		if tree.Digest == "" {
			b.Fatal("missing tree")
		}
		cleanup()
	}
}
