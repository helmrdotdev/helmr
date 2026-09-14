package deployment

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestInputTreeDirectoryAndArtifactHaveOneIdentity(t *testing.T) {
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
	directoryDigest, err := ProgramInputTreeDigest(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest, err := inputTreeDigest(t.Context(), artifact.entries, artifact.Open)
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
			changed, err := inputTreeDigest(t.Context(), artifact.entries, artifact.Open)
			if err != nil {
				t.Fatal(err)
			}
			if changed == archiveDigest {
				t.Fatal("input mutation not bound")
			}
			archiveDigest = changed
		})
	}
	artifact.addDirectory("helmr")
	artifact.addFile("helmr/config.json", []byte("{}"), 0644)
	generated, err := inputTreeDigest(context.Background(), artifact.entries, artifact.Open)
	if err != nil || generated != archiveDigest {
		t.Fatalf("generated metadata entered input digest: %s %v", generated, err)
	}
}

func TestSourceAdmissionRejectsCanonicalRootConfigAndDormantTampering(t *testing.T) {
	p := newTestProgram(t)
	p.artifact.replaceFile("tasks/build.ts", []byte("export default {}"))
	p.refreshManifest(t)
	delete(p.artifact.files, "helmr.config.ts")
	p.artifact.entries = slices.DeleteFunc(p.artifact.entries, func(e artifactEntry) bool { return e.Path == "helmr.config.ts" })
	p.artifact.addLink("helmr.config.ts", "tasks/build.ts")
	p.refreshManifest(t)
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("canonical root config used as declaration")
	}
	p = newTestProgram(t)
	p.artifact.addFile("dormant.txt", []byte("before"), 0644)
	p.refreshManifest(t)
	p.artifact.replaceFile("dormant.txt", []byte("after!"))
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("dormant tampering accepted")
	}
}

func TestInputTreeAcceptsLargeDormantInstalledBinary(t *testing.T) {
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
	if _, err := ProgramInputTreeDigest(t.Context(), root); err != nil {
		t.Fatal(err)
	}
}

func TestInputTreeRejectsInvalidLinkBytesBeforeHash(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink(string([]byte{0xff}), filepath.Join(root, "bad-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ProgramInputTreeDigest(t.Context(), root); err == nil {
		t.Fatal("accepted invalid link bytes")
	}
}
