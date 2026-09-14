package deployment

import (
	"bytes"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestBuildConfigCompilePackagesCanonicalAuthority(t *testing.T) {
	raw := []byte(`{"compilePackages":["node_modules/@s/a","node_modules/z"],"dirs":["tasks"],"ignorePatterns":[]}`)
	config, err := ParseBuildConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := CanonicalBuildConfig(config)
	if err != nil || !bytes.Equal(raw, encoded) {
		t.Fatalf("canonical: %s %v", encoded, err)
	}
	cloned := cloneBuildConfig(config)
	before, _ := BuildConfigDigest(config)
	config.CompilePackages[0] = "node_modules/@s/b"
	after, _ := BuildConfigDigest(config)
	if before == after || cloned.CompilePackages[0] != "node_modules/@s/a" {
		t.Fatal("selection not bound or clone shared")
	}
}

func TestBuildConfigRejectsMalformedCompileSelection(t *testing.T) {
	for _, selection := range []string{`null`, `"a"`, `[null]`, `[4]`, `["node_modules/a","node_modules/a"]`, `["node_modules/z","node_modules/a"]`, `["./node_modules/a"]`, `["node_modules/a/lib"]`, `["../node_modules/a"]`, `["node_modules/a/.helmr/node_modules/b"]`, `["helmr/node_modules/a"]`, `["node_modules/a\ud800"]`} {
		t.Run(selection, func(t *testing.T) {
			raw := []byte(`{"compilePackages":` + selection + `,"dirs":["tasks"],"ignorePatterns":[]}`)
			// Valid JSON is canonicalized so rejection tests the config contract, not spacing.
			if canonical, err := jsoncanon.Transform(raw); err == nil {
				raw = canonical
			}
			if _, err := ParseBuildConfig(raw); err == nil {
				t.Fatal("accepted malformed selection")
			}
		})
	}
	for _, raw := range []string{`{"dirs":["tasks"],"ignorePatterns":[]}`, `{"compilePackages":[],"compilePackages":[],"dirs":["tasks"],"ignorePatterns":[]}`, `{"compilePackages":[],"dirs":["tasks"],"helmr":{},"ignorePatterns":[]}`} {
		if _, err := ParseBuildConfig([]byte(raw)); err == nil {
			t.Fatal("accepted missing, duplicate or unknown field")
		}
	}
}

func TestArtifactSelectionMatchesEvaluatedConfigWithoutPackageJSONAuthority(t *testing.T) {
	p := selectedTestProgram(t)
	p.artifact.replaceFile("package.json", []byte(`{"helmr":{"compilePackages":["node_modules/not-selected"]}}`))
	if err := verifyProgramCompilePackages(t.Context(), mustInspectTestProgram(t, p), p.manifest.CompilePackages, p.manifest.Config); err != nil {
		t.Fatal(err)
	}
	p.manifest.CompilePackages[0].LogicalRoot = "node_modules/different"
	if err := verifyProgramCompilePackages(t.Context(), mustInspectTestProgram(t, p), p.manifest.CompilePackages, p.manifest.Config); err == nil || !strings.Contains(err.Error(), "evaluated config") {
		t.Fatalf("unbound selection: %v", err)
	}
}

func mustInspectTestProgram(t *testing.T, p *testProgram) *inspectedArtifact {
	t.Helper()
	artifact, err := inspectArtifact(t.Context(), p.artifact, buildTreeArtifact, maxBuildTreeLogicalBytes, squashFSPhysicalAlign)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
