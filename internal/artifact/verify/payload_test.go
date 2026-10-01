package verify

import (
	"testing"
)

func TestPayloadAdmissionRejectsModuleLinksAndDormantTampering(t *testing.T) {
	p := newTestProgram(t)
	p.artifact.Remove("helmr/app/entry-0.mjs")
	p.artifact.AddFile("other.mjs", []byte("export default {}"), 0644)
	p.artifact.AddLink("helmr/app/entry-0.mjs", "../../other.mjs")
	p.refreshManifest(t)
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("generated module symlink accepted")
	}
	p = newTestProgram(t)
	p.artifact.AddFile("dormant.txt", []byte("before"), 0644)
	p.refreshManifest(t)
	p.artifact.ReplaceFile("dormant.txt", []byte("after!"))
	if _, err := verifyProgramArtifact(t.Context(), p.descriptor); err == nil {
		t.Fatal("dormant tampering accepted")
	}
}

func TestPayloadAdmissionBindsGeneratedCodeAndAssetResolution(t *testing.T) {
	for _, name := range []string{"helmr/app/entry-0.mjs", "package.json", "prompt.md"} {
		t.Run(name, func(t *testing.T) {
			program := newTestProgram(t)
			if name == "prompt.md" {
				program.artifact.AddFile(name, []byte("original prompt"), 0644)
			}
			program.refreshManifest(t)
			program.artifact.ReplaceFile(name, []byte("changed payload"))
			if _, err := verifyProgramArtifact(t.Context(), program.descriptor); err == nil {
				t.Fatal("payload mutation was accepted")
			}
		})
	}
}
