package deployment

import (
	"reflect"
	"strings"
	"testing"
)

func testCompilerInputs() CompilerInputs {
	return CompilerInputs{
		APIVersion:            "helmr.compiler.v0",
		ConfigEvaluator:       CompilerEntrypoint{APIVersion: ConfigEvaluatorContract, Digest: testDigest("config evaluator"), Entrypoint: "/nix/helmr/config-evaluator.mjs"},
		Esbuild:               EsbuildInputs{APIPackageDigest: testDigest("esbuild api"), BinaryDigest: testDigest("esbuild binary"), BinaryPath: "/nix/helmr/esbuild", PackagePath: "/nix/node_modules/esbuild", Version: "0.28.1"},
		OptionsContractDigest: testDigest("compiler options contract"),
		Output:                CompilerOutputContract{Aggregate: "analysis-only", FinalModules: "independent", SourceMaps: "external"},
		ProgramCompiler:       CompilerEntrypoint{APIVersion: "helmr.compiler.v0", Digest: testDigest("program compiler"), Entrypoint: "/nix/helmr/program-compiler.mjs"},
		Source:                CompilerSourceContract{DeclarationExtensions: []string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"}, PackageDependencies: "external", Semantics: "pinned-esbuild", WorkspaceDependencies: "bundled"},
	}
}

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

func TestProgramCompilerSelectionsUseDeclarationOrder(t *testing.T) {
	task := ProgramCompilerSelection{Kind: DeclarationKindTask, DeclaredID: "z-task"}
	actor := ProgramCompilerSelection{Kind: DeclarationKindActor, DeclaredID: "a-actor"}
	if compareProgramCompilerSelection(task, actor) >= 0 {
		t.Fatal("task selection did not sort before actor selection")
	}
}
