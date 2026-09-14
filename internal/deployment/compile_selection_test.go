package deployment

import (
	"context"
	"encoding/json"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"strings"
	"testing"
)

func TestInstalledPackageRoot(t *testing.T) {
	for value, want := range map[string]string{
		"tasks/a.ts":              "",
		"node_modules/a/lib/a.ts": "node_modules/a",
		"node_modules/a/node_modules/@s/b/lib/a.ts":           "node_modules/a/node_modules/@s/b",
		"node_modules/.pnpm/a@1/node_modules/a/dist/index.ts": "node_modules/.pnpm/a@1/node_modules/a",
		"node_modules/@s/a/node_modules/b/a.ts":               "node_modules/@s/a/node_modules/b",
	} {
		if got := installedPackageRoot(value); got != want {
			t.Errorf("installedPackageRoot(%q) = %q, want %q", value, got, want)
		}
	}
	selection := ProgramCompileSelection{Packages: []ProgramCompilePackage{{ResolvedRoot: "node_modules/a"}}}
	for _, input := range []string{"node_modules/a/node_modules/b/index.ts", "node_modules/ab/index.ts"} {
		if selectedPackageContains(selection, input) {
			t.Fatalf("parent selection authorized %q", input)
		}
	}
	if !selectedPackageContains(selection, "node_modules/a/dist/a.ts") {
		t.Fatal("export subdirectory was rejected")
	}
}

func TestManifestCompilePackagesStrictSchema(t *testing.T) {
	for _, raw := range []string{
		"\ufeff{}", `{"unused":1e400}`, `{"unused":"\ud800"}`, `{/*comment*/"helmr":{}}`,
		`null`, `[]`, `{"helmr":null}`, `{"helmr":[]}`, `{"helmr":{"unknown":[]}}`,
		`{"helmr":{"compilePackages":null}}`, `{"helmr":{"compilePackages":"node_modules/a"}}`,
		`{"helmr":{"compilePackages":[null]}}`, `{"helmr":{"compilePackages":[4]}}`,
		`{"helmr":{},"helmr":{}}`, `{"helmr":{"compilePackages":[],"compilePackages":[]}}`,
		`{"helmr":{"compilePackages":["node_modules/a","node_modules/a"]}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := manifestCompilePackages([]byte(raw)); err == nil {
				t.Fatal("accepted malformed policy")
			}
		})
	}
	for _, selector := range []string{"../node_modules/a", "/node_modules/a", "node_modules/a/", "node_modules/a/lib", "node_modules//a", "node_modules/./a", "node_modules/a\\b", "node_modules/a\x00", "helmr/node_modules/a", "node_modules/a/.helmr/node_modules/b"} {
		raw, _ := json.Marshal(map[string]any{"helmr": map[string]any{"compilePackages": []string{selector}}})
		if _, err := manifestCompilePackages(raw); err == nil {
			t.Fatalf("accepted invalid selector %q", selector)
		}
	}
	for _, raw := range []string{`{}`, `{"helmr":{}}`, `{"optionalDependencies":{"missing":"file:missing"},"workspaces":["missing/*"]}`} {
		if got, err := manifestCompilePackages([]byte(raw)); err != nil || len(got) != 0 {
			t.Fatalf("unused declarations affected policy: %v %v", got, err)
		}
	}
}

func selectedTestProgram(t *testing.T) *testProgram {
	t.Helper()
	program := newTestProgram(t)
	raw := []byte(`{"helmr":{"compilePackages":["node_modules/alias"]}}`)
	program.artifact.replaceFile("package.json", raw)
	program.artifact.addDirectory("node_modules/.store")
	program.artifact.addDirectory("node_modules/.store/actual@2")
	program.artifact.addDirectory("node_modules/.store/actual@2/node_modules")
	target := "node_modules/.store/actual@2/node_modules/actual"
	program.artifact.addDirectory(target)
	program.artifact.addFile(target+"/package.json", []byte(`{"name":"actual","version":"2.0.0"}`), 0644)
	program.artifact.addFile(target+"/index.ts", []byte(`export const value: string = "installed"`), 0644)
	program.artifact.addLink("node_modules/alias", ".store/actual@2/node_modules/actual")
	program.manifest.CompileSelection = ProgramCompileSelection{
		PackageJSONDigest: testDigest(string(raw)), Packages: []ProgramCompilePackage{{LogicalRoot: "node_modules/alias", ResolvedRoot: target}},
	}
	input := ProgramPathDigest{Path: target + "/index.ts", Digest: testDigest(string(program.artifact.files[target+"/index.ts"]))}
	program.manifest.CompiledInputs = append([]ProgramPathDigest{input}, program.manifest.CompiledInputs...)
	program.refreshManifest(t)
	return program
}

func TestCompileSelectionArtifactBinding(t *testing.T) {
	program := selectedTestProgram(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*testProgram){
		"root manifest bytes": func(p *testProgram) { p.artifact.replaceFile("package.json", []byte(`{}`)) },
		"selector dropped":    func(p *testProgram) { p.manifest.CompileSelection.Packages = []ProgramCompilePackage{} },
		"digest":              func(p *testProgram) { p.manifest.CompileSelection.PackageJSONDigest = testDigest("tampered") },
		"declared instance":   func(p *testProgram) { p.manifest.CompileSelection.Packages[0].ResolvedRoot = "node_modules/other" },
		"retarget alias": func(p *testProgram) {
			p.artifact.addDirectory("node_modules/other")
			p.artifact.mutate("node_modules/alias", func(entry *artifactEntry) { entry.LinkTarget = "other"; entry.SizeBytes = int64(len(entry.LinkTarget)) })
		},
		"escape alias": func(p *testProgram) {
			p.artifact.mutate("node_modules/alias", func(entry *artifactEntry) {
				entry.LinkTarget = "../../outside"
				entry.SizeBytes = int64(len(entry.LinkTarget))
			})
		},
		"selected package missing": func(p *testProgram) {
			delete(p.artifact.files, p.manifest.CompileSelection.Packages[0].ResolvedRoot+"/package.json")
		},
		"selected package null": func(p *testProgram) {
			p.artifact.replaceFile(p.manifest.CompileSelection.Packages[0].ResolvedRoot+"/package.json", []byte("null"))
		},
		"selected package duplicate JSON": func(p *testProgram) {
			p.artifact.replaceFile(p.manifest.CompileSelection.Packages[0].ResolvedRoot+"/package.json", []byte(`{"name":"a","name":"b"}`))
		},
		"compiled input bytes":                  func(p *testProgram) { p.artifact.replaceFile(p.manifest.CompiledInputs[0].Path, []byte("changed")) },
		"compiled source absent from input set": func(p *testProgram) { p.manifest.CompiledInputs = p.manifest.CompiledInputs[:1] },
	} {
		t.Run(name, func(t *testing.T) {
			p := selectedTestProgram(t)
			mutate(p)
			// Tampered shape can be rejected while encoding, before artifact admission.
			raw, err := canonicalProgramManifest(p.manifest)
			if err != nil {
				return
			}
			p.artifact.replaceFile("helmr/program-manifest.json", raw)
			if _, err := verifyProgramArtifact(context.Background(), p.descriptor); err == nil {
				t.Fatal("accepted tampered evidence")
			}
		})
	}
}

func TestCompileSelectionRejectsNestedInputsAndLegacyEvidence(t *testing.T) {
	result := testProgramCompilerResult(t)
	result.CompileSelection.Packages = []ProgramCompilePackage{{LogicalRoot: "node_modules/a", ResolvedRoot: "node_modules/a"}}
	result.Inputs = []ProgramPathDigest{{Path: "node_modules/a/node_modules/@s/b/index.ts", Digest: testDigest("child")}}
	if err := validateProgramCompilerResult(result); err == nil || !strings.Contains(err.Error(), "selected package") {
		t.Fatalf("nested input: %v", err)
	}
	manifest := testProgramManifest(t)
	manifest.CompileSelection = result.CompileSelection
	manifest.CompiledInputs = result.Inputs
	if err := validateProgramManifest(manifest); err == nil {
		t.Fatal("manifest allowed nested input")
	}
	for _, extension := range []string{".ts", ".tsx", ".mts", ".cts"} {
		edge := ProgramExternalEdge{Importer: "tasks/a.ts", Kind: "import-statement", Specifier: "a", LogicalPath: "node_modules/a/index" + extension, ResolvedPath: "node_modules/a/index" + extension, RuntimePath: "/opt/helmr/program/node_modules/a/index" + extension}
		if err := validateProgramExternalEdge(edge); err == nil {
			t.Fatalf("external %s accepted", extension)
		}
	}
	result = testProgramCompilerResult(t)
	raw, _ := canonicalProgramCompilerResult(result)
	raw = append([]byte(`{"localPackages":[],`), raw[1:]...)
	raw, _ = jsoncanon.Transform(raw)
	if _, err := ParseProgramCompilerResult(raw); err == nil {
		t.Fatal("legacy compiler evidence accepted")
	}
	manifest = testProgramManifest(t)
	raw, _ = canonicalProgramManifest(manifest)
	raw = append([]byte(`{"localPackages":[],`), raw[1:]...)
	raw, _ = jsoncanon.Transform(raw)
	if _, err := ParseProgramManifest(raw); err == nil {
		t.Fatal("legacy manifest evidence accepted")
	}
}

func TestCompilerResultAndManifestVerifySameSelectedInputs(t *testing.T) {
	program := selectedTestProgram(t)
	ctx := context.Background()
	artifact, err := inspectArtifact(ctx, program.artifact, buildTreeArtifact, maxBuildTreeLogicalBytes, squashFSPhysicalAlign)
	if err != nil {
		t.Fatal(err)
	}
	result := testProgramCompilerResult(t)
	result.Config = program.manifest.Config
	result.CompileSelection = program.manifest.CompileSelection
	result.Inputs = program.manifest.CompiledInputs
	result.Outputs = program.manifest.Modules
	if err := verifyProgramCompilerFiles(ctx, artifact, result); err != nil {
		t.Fatal(err)
	}
	projected := programManifestFromCompilerResult(result, program.manifest.ProgramIndexDigest)
	if err := verifyProgramManifestFiles(ctx, artifact, projected); err != nil {
		t.Fatal(err)
	}
	result.Inputs[0].Digest = testDigest("wrong installed bytes")
	if err := verifyProgramCompilerFiles(ctx, artifact, result); err == nil {
		t.Fatal("compiler result accepted wrong installed input digest")
	}
	// Conversion owns its slices and retains the complete actual-input evidence.
	if err := verifyProgramManifestFiles(ctx, artifact, projected); err != nil {
		t.Fatalf("manifest clone changed with producer: %v", err)
	}
	projected.CompiledInputs = projected.CompiledInputs[:1]
	if err := verifyProgramManifestFiles(ctx, artifact, projected); err == nil {
		t.Fatal("source map accepted a source absent from compiled inputs")
	}
}

func TestCompileSelectionRetainsUnusedAliases(t *testing.T) {
	program := selectedTestProgram(t)
	target := program.manifest.CompileSelection.Packages[0].ResolvedRoot
	program.artifact.addLink("node_modules/z", ".store/actual@2/node_modules/actual")
	raw := []byte(`{"helmr":{"compilePackages":["node_modules/z","node_modules/alias"]}}`)
	program.artifact.replaceFile("package.json", raw)
	program.manifest.CompileSelection.PackageJSONDigest = testDigest(string(raw))
	program.manifest.CompileSelection.Packages = append(program.manifest.CompileSelection.Packages, ProgramCompilePackage{LogicalRoot: "node_modules/z", ResolvedRoot: target})
	program.refreshManifest(t)
	if _, err := verifyProgramArtifact(context.Background(), program.descriptor); err != nil {
		t.Fatal(err)
	}
}
