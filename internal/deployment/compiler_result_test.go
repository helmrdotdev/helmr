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

func TestProgramCompilerResultRoundTrip(t *testing.T) {
	result := testProgramCompilerResult(t)
	canonical, err := canonicalProgramCompilerResult(result)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramCompilerResult(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, result) {
		t.Fatalf("parsed result = %#v, want %#v", parsed, result)
	}
}

func testProgramCompilerResult(t *testing.T) ProgramCompilerResult {
	t.Helper()
	compiler := testCompilerInputs()
	optionsDigest, err := compilerOptionsDigest(compiler, "24.16.0")
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := "tasks/build.ts"
	modulePath := generatedDeclarationModulePath(sourcePath)
	return ProgramCompilerResult{
		AggregateResultDigest: "sha256:" + strings.Repeat("a", 64),
		Compiler: ProgramCompilerContract{
			APIVersion:            compiler.APIVersion,
			EsbuildVersion:        compiler.Esbuild.Version,
			OptionsContractDigest: compiler.OptionsContractDigest,
			Output:                compiler.Output,
			Source:                compiler.Source,
		},
		Config: ProgramPathDigest{
			Digest: "sha256:" + strings.Repeat("4", 64),
			Path:   "helmr/config.json",
		},
		DiscoveryCandidates: []string{sourcePath},
		Execution: ProgramCompilerExecution{
			NodeVersion:   "24.16.0",
			OptionsDigest: optionsDigest,
		},
		ExternalEdges: []ProgramExternalEdge{},
		Inputs: []ProgramPathDigest{{
			Digest: "sha256:" + strings.Repeat("9", 64),
			Path:   sourcePath,
		}},
		LocalPackages: []ProgramLocalPackage{},
		Outputs: []ProgramModule{{
			ModuleDigest:    "sha256:" + strings.Repeat("b", 64),
			ModulePath:      modulePath,
			SourceMapDigest: "sha256:" + strings.Repeat("c", 64),
			SourceMapPath:   modulePath + ".map",
			SourcePath:      sourcePath,
		}},
		Selections: []ProgramCompilerSelection{{
			DeclaredID: "build",
			ExportName: "build",
			Kind:       DeclarationKindTask,
			SourcePath: sourcePath,
			Slot:       DeclarationSlotHandler,
		}},
		TSConfigs: []ProgramPathDigest{},
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
