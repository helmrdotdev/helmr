package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

func TestPayloadAcceptsLargeDormantInstalledBinary(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(artifact.MaxProgramFileSizeBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ProgramPayloadDigest(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestPayloadRejectsInvalidLinkBytesBeforeHash(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(string([]byte{0xff}), filepath.Join(root, "bad-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ProgramPayloadDigest(t.Context(), root); err == nil {
		t.Fatal("accepted invalid link bytes")
	}
}

func TestPayloadDirectoryAndArtifactHaveOneIdentity(t *testing.T) {
	root := t.TempDir()
	memory := newMemoryArtifact()
	files := map[string]string{"package.json": "{}", "main.ts": "export default 1", "unused.txt": "asset"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		memory.addFile(name, []byte(body), 0644)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	memory.addDirectory("empty")
	if err := os.Symlink("unused.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	memory.addLink("link", "unused.txt")
	directoryDigest, err := ProgramPayloadDigest(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest, err := artifact.PayloadDigest(t.Context(), memory.entries, memory.Open)
	if err != nil {
		t.Fatal(err)
	}
	if directoryDigest != archiveDigest {
		t.Fatalf("directory %s != archive %s", directoryDigest, archiveDigest)
	}
	// Preserve the accepted identity. Every input axis, including dormant bytes,
	// must invalidate it; generated metadata is the sole excluded namespace.
	for name, change := range map[string]func(){
		"dormant bytes":   func() { memory.replaceFile("unused.txt", []byte("other")) },
		"executable mode": func() { memory.mutate("main.ts", func(e *artifact.Entry) { e.Mode = 0755 }) },
		"symlink target":  func() { memory.mutate("link", func(e *artifact.Entry) { e.LinkTarget = "main.ts" }) },
		"directory":       func() { memory.addDirectory("another") },
	} {
		t.Run(name, func(t *testing.T) {
			change()
			changed, err := artifact.PayloadDigest(t.Context(), memory.entries, memory.Open)
			if err != nil {
				t.Fatal(err)
			}
			if changed == archiveDigest {
				t.Fatal("payload mutation not bound")
			}
			archiveDigest = changed
		})
	}
	memory.addDirectory("helmr")
	memory.addFile("helmr/config.json", []byte("{}"), 0644)
	generated, err := artifact.PayloadDigest(context.Background(), memory.entries, memory.Open)
	if err != nil || generated != archiveDigest {
		t.Fatalf("generated metadata entered input digest: %s %v", generated, err)
	}
}
