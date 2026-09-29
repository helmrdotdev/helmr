package verify

import (
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/artifact"
)

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
