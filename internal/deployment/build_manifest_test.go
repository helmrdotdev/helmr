package deployment

import (
	"reflect"
	"strings"
	"testing"
)

func TestProgramVerificationRoundTrip(t *testing.T) {
	canonical := canonicalVerifierProgramVerification(t)
	verified, err := parseProgramVerification(canonical)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := canonicalProgramVerification(verified)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reencoded, canonical) {
		t.Fatalf("reencoded verification = %q, want %q", reencoded, canonical)
	}
}

func TestProgramBuildManifestBindsProtectedInputs(t *testing.T) {
	manifest := testProgramBuildManifest(t)
	canonical, err := canonicalProgramBuildManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramBuildManifest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatalf("parsed manifest = %#v, want %#v", parsed, manifest)
	}

	tests := map[string]func(*ProgramBuildManifest){
		"config source path": func(value *ProgramBuildManifest) {
			value.ConfigSource.Path = "config.ts"
		},
		"config source digest": func(value *ProgramBuildManifest) {
			value.ConfigSource.Digest = "invalid"
		},
		"lockfile path": func(value *ProgramBuildManifest) {
			value.Lockfile.Path = "lock.json"
		},
		"lockfile digest": func(value *ProgramBuildManifest) {
			value.Lockfile.Digest = "invalid"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := manifest
			mutate(&value)
			if err := validateProgramBuildManifest(value); err == nil {
				t.Fatal("validateProgramBuildManifest returned nil error")
			}
		})
	}
}

func testProgramBuildManifest(t *testing.T) ProgramBuildManifest {
	t.Helper()
	indexRaw, err := CanonicalProgramIndex(testProgramIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	compiler := testCompilerInputs()
	optionsDigest, err := compilerOptionsDigest(compiler, "24.16.0")
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := "tasks/build.ts"
	modulePath := generatedDeclarationModulePath(sourcePath)
	return ProgramBuildManifest{
		AggregateResultDigest: "sha256:" + strings.Repeat("a", 64),
		Compiler: ProgramCompilerContract{
			APIVersion:            compiler.APIVersion,
			EsbuildVersion:        compiler.Esbuild.Version,
			OptionsContractDigest: compiler.OptionsContractDigest,
			Output:                compiler.Output,
			Source:                compiler.Source,
		},
		Config: ProgramBuildFile{
			Digest: "sha256:" + strings.Repeat("4", 64),
			Path:   "helmr/config.json",
		},
		ConfigSource: ProgramBuildFile{
			Digest: "sha256:" + strings.Repeat("5", 64),
			Path:   "helmr.config.ts",
		},
		DiscoveryCandidates: []string{sourcePath},
		Execution: ProgramBuildExecution{
			NodeVersion:   "24.16.0",
			OptionsDigest: optionsDigest,
		},
		ExternalEdges: []ProgramBuildExternalEdge{},
		Inputs: []ProgramBuildFile{{
			Digest: "sha256:" + strings.Repeat("9", 64),
			Path:   sourcePath,
		}},
		LocalPackages: []ProgramBuildLocalPackage{},
		Lockfile: ProgramBuildFile{
			Digest: "sha256:" + strings.Repeat("6", 64),
			Path:   "bun.lock",
		},
		Outputs: []ProgramBuildOutput{{
			ModuleDigest:    "sha256:" + strings.Repeat("b", 64),
			ModulePath:      modulePath,
			SourceMapDigest: "sha256:" + strings.Repeat("c", 64),
			SourceMapPath:   modulePath + ".map",
			SourcePath:      sourcePath,
		}},
		ProgramIndexDigest: testDigest(string(indexRaw)),
		Selections: []ProgramBuildSelection{{
			DeclaredID: "build",
			ExportName: "build",
			Kind:       DeclarationKindTask,
			SourcePath: sourcePath,
			Slot:       DeclarationSlotHandler,
		}},
		TSConfigs: []ProgramBuildFile{},
	}
}

func testProgramOutput(t *testing.T) ProgramOutput {
	t.Helper()
	return ProgramOutput{
		Artifact: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("c", 64),
			SizeBytes: squashFSPhysicalAlign,
			MediaType: ProgramArtifactMediaType,
		},
		Index: testProgramIndex(t),
	}
}
