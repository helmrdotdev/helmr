package deployment

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPayloadDirectoryAndArtifactHaveOneIdentity(t *testing.T) {
	root := t.TempDir()
	artifact := newMemoryArtifact()
	files := map[string]string{"package.json": "{}", "main.ts": "export default 1", "unused.txt": "asset"}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		artifact.addFile(name, []byte(body), 0644)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	artifact.addDirectory("empty")
	if err := os.Symlink("unused.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	artifact.addLink("link", "unused.txt")
	directoryDigest, err := ProgramPayloadDigest(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest, err := payloadDigest(t.Context(), artifact.entries, artifact.Open)
	if err != nil {
		t.Fatal(err)
	}
	if directoryDigest != archiveDigest {
		t.Fatalf("directory %s != archive %s", directoryDigest, archiveDigest)
	}
	// Preserve the accepted identity. Every input axis, including dormant bytes,
	// must invalidate it; generated metadata is the sole excluded namespace.
	for name, change := range map[string]func(){
		"dormant bytes":   func() { artifact.replaceFile("unused.txt", []byte("other")) },
		"executable mode": func() { artifact.mutate("main.ts", func(e *artifactEntry) { e.Mode = 0755 }) },
		"symlink target":  func() { artifact.mutate("link", func(e *artifactEntry) { e.LinkTarget = "main.ts" }) },
		"directory":       func() { artifact.addDirectory("another") },
	} {
		t.Run(name, func(t *testing.T) {
			change()
			changed, err := payloadDigest(t.Context(), artifact.entries, artifact.Open)
			if err != nil {
				t.Fatal(err)
			}
			if changed == archiveDigest {
				t.Fatal("payload mutation not bound")
			}
			archiveDigest = changed
		})
	}
	artifact.addDirectory("helmr")
	artifact.addFile("helmr/config.json", []byte("{}"), 0644)
	generated, err := payloadDigest(context.Background(), artifact.entries, artifact.Open)
	if err != nil || generated != archiveDigest {
		t.Fatalf("generated metadata entered input digest: %s %v", generated, err)
	}
}

func TestPayloadAdmissionRejectsModuleLinksAndDormantTampering(t *testing.T) {
	p := newTestProgram(t)
	delete(p.artifact.files, "helmr/app/entry-0.mjs")
	p.artifact.entries = slices.DeleteFunc(p.artifact.entries, func(e artifactEntry) bool { return e.Path == "helmr/app/entry-0.mjs" })
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

func TestPayloadAcceptsLargeDormantInstalledBinary(t *testing.T) {
	root := t.TempDir()
	file, err := os.Create(filepath.Join(root, "binary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxProgramFileSizeBytes + 1); err != nil {
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
