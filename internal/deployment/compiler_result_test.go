package deployment

import (
	"encoding/json"
	"maps"
	"reflect"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func testLanguageIdentity() ModuleExecutionIdentity {
	return ModuleExecutionIdentity{APIVersion: "helmr.module-execution.v1", AdapterDigest: testDigest("adapter"), TypeScriptDigest: testDigest("typescript"), TypeScriptVersion: "6.0.3"}
}
func testCompilerInputs() CompilerInputs {
	return CompilerInputs{APIVersion: "helmr.compiler.v1", Language: testLanguageIdentity(),
		ConfigEvaluator: CompilerEntrypoint{APIVersion: ConfigEvaluatorContract, Digest: testDigest("config evaluator"), Entrypoint: "/nix/helmr/config-evaluator.mjs"},
		ProgramCompiler: CompilerEntrypoint{APIVersion: "helmr.compiler.v1", Digest: testDigest("program compiler"), Entrypoint: "/nix/helmr/program-compiler.mjs"}}
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
	return ProgramCompilerResult{APIVersion: "helmr.compiler.v1", Language: testLanguageIdentity(), NodeVersion: "24.21.0", Config: ProgramPathDigest{Path: "helmr/config.json", Digest: testDigest("config")}, InputTreeDigest: testDigest("input"), DiscoveryCandidates: []string{"tasks/build.ts"}, Selections: []ProgramCompilerSelection{{DeclaredID: "build", ExportName: "build", Kind: DeclarationKindTask, SourcePath: "tasks/build.ts", Slot: DeclarationSlotHandler}}}
}

func TestProgramCompilerSelectionsUseDeclarationOrder(t *testing.T) {
	task := ProgramCompilerSelection{Kind: DeclarationKindTask, DeclaredID: "z-task"}
	actor := ProgramCompilerSelection{Kind: DeclarationKindActor, DeclaredID: "a-actor"}
	if compareProgramCompilerSelection(task, actor) >= 0 {
		t.Fatal("task selection did not sort before actor selection")
	}
}

func TestCompilerAuthorityMismatchTuples(t *testing.T) {
	for _, mutate := range []func(*ProgramCompilerResult){
		func(v *ProgramCompilerResult) { v.Language.AdapterDigest = testDigest("changed") },
		func(v *ProgramCompilerResult) { v.Language.TypeScriptDigest = testDigest("changed") },
		func(v *ProgramCompilerResult) { v.Language.TypeScriptVersion = "7.0.2" },
		func(v *ProgramCompilerResult) { v.NodeVersion = "24.20.0" },
		func(v *ProgramCompilerResult) { v.APIVersion = "helmr.compiler.v0" },
	} {
		v := testProgramCompilerResult(t)
		mutate(&v)
		if err := validateProgramCompilerAuthority(v, testCompilerInputs(), "24.21.0"); err == nil {
			t.Fatal("accepted mismatched authority")
		}
	}
}

func TestNativeCompilerContractsRejectOpenMissingAndOldShapes(t *testing.T) {
	result := testProgramCompilerResult(t)
	resultRaw, err := canonicalProgramCompilerResult(result)
	if err != nil {
		t.Fatal(err)
	}
	compilerRaw, err := CanonicalCompilerInputs(testCompilerInputs())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		raw   []byte
		parse func([]byte) error
	}{
		{resultRaw, func(raw []byte) error { _, err := ParseProgramCompilerResult(raw); return err }},
		{compilerRaw, func(raw []byte) error { _, err := ParseCompilerInputs(raw); return err }},
	}
	for _, item := range cases {
		var document map[string]json.RawMessage
		if err := json.Unmarshal(item.raw, &document); err != nil {
			t.Fatal(err)
		}
		for key := range document {
			candidate := maps.Clone(document)
			delete(candidate, key)
			encoded, _ := json.Marshal(candidate)
			canonical, err := jsoncanon.Transform(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if item.parse(canonical) == nil {
				t.Fatalf("accepted missing %s in %s", key, item.raw)
			}
		}
		for key, value := range map[string]json.RawMessage{"outputs": json.RawMessage(`[]`), "compilePackages": json.RawMessage(`[]`), "apiVersion": json.RawMessage(`"helmr.compiler.v0"`)} {
			candidate := maps.Clone(document)
			candidate[key] = value
			encoded, _ := json.Marshal(candidate)
			canonical, err := jsoncanon.Transform(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if item.parse(canonical) == nil {
				t.Fatalf("accepted stale/open %s", key)
			}
		}
	}
	for _, mutate := range []func(*ProgramCompilerResult){
		func(r *ProgramCompilerResult) { r.DiscoveryCandidates = nil },
		func(r *ProgramCompilerResult) { r.Selections = nil },
		func(r *ProgramCompilerResult) { r.Selections[0].Slot = "other" },
		func(r *ProgramCompilerResult) { r.Selections[0].SourcePath = "tasks/missing.ts" },
		func(r *ProgramCompilerResult) { r.DiscoveryCandidates = []string{"tasks/build.ts", "tasks/build.ts"} },
	} {
		candidate := testProgramCompilerResult(t)
		mutate(&candidate)
		if err := validateProgramCompilerResult(candidate); err == nil {
			t.Fatal("accepted malformed compiler evidence")
		}
	}
}
