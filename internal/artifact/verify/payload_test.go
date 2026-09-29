package verify

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

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
	directoryDigest, err := artifact.ProgramPayloadDigest(t.Context(), root)
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

func TestPayloadAdmissionRejectsModuleLinksAndDormantTampering(t *testing.T) {
	p := newTestProgram(t)
	delete(p.artifact.files, "helmr/app/entry-0.mjs")
	p.artifact.entries = slices.DeleteFunc(p.artifact.entries, func(e artifact.Entry) bool { return e.Path == "helmr/app/entry-0.mjs" })
	p.artifact.addFile("other.mjs", []byte("export default {}"), 0644)
	p.artifact.addLink("helmr/app/entry-0.mjs", "../../other.mjs")
	p.refreshManifest(t)
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("generated module symlink accepted")
	}
	p = newTestProgram(t)
	p.artifact.addFile("dormant.txt", []byte("before"), 0644)
	p.refreshManifest(t)
	p.artifact.replaceFile("dormant.txt", []byte("after!"))
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("dormant tampering accepted")
	}
}

func TestPayloadAdmissionBindsGeneratedCodeAndAssetResolution(t *testing.T) {
	for _, name := range []string{"helmr/app/entry-0.mjs", "package.json", "prompt.md"} {
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			if name == "prompt.md" {
				program.artifact.addFile(name, []byte("original prompt"), 0644)
			}
			program.refreshManifest(t)
			program.artifact.replaceFile(name, []byte("changed payload"))
			if _, err := verifyProgramArtifact(t.Context(), program.descriptor); err == nil {
				t.Fatal("payload mutation was accepted")
			}
		})
	}
}
