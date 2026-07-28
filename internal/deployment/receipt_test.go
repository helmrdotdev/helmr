package deployment

import (
	"reflect"
	"strings"
	"testing"
)

func TestProgramReceiptRoundTrip(t *testing.T) {
	receipt := testProgramReceipt(t)
	canonical, err := CanonicalProgramReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramReceipt(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, receipt) {
		t.Fatalf("parsed receipt = %#v, want %#v", parsed, receipt)
	}
}

func TestProgramReceiptRejectsInvalidAuthority(t *testing.T) {
	base := testProgramReceipt(t)
	tests := map[string]func(*ProgramReceipt){
		"format version": func(receipt *ProgramReceipt) {
			receipt.FormatVersion = 1
		},
		"compiler binary digest": func(receipt *ProgramReceipt) {
			receipt.Compiler.BinaryDigest = "sha256:" + strings.Repeat("A", 64)
		},
		"manifest digest": func(receipt *ProgramReceipt) {
			receipt.Program.ManifestDigest = "sha256:invalid"
		},
		"program media type": func(receipt *ProgramReceipt) {
			receipt.Program.MediaType = "application/octet-stream"
		},
		"index digest": func(receipt *ProgramReceipt) {
			receipt.Program.IndexDigest = "sha256:invalid"
		},
		"runtime": func(receipt *ProgramReceipt) {
			receipt.Runtime.APIVersion = "helmr.runtime.v1"
		},
		"source": func(receipt *ProgramReceipt) {
			receipt.Source.Digest = "invalid"
		},
		"source size": func(receipt *ProgramReceipt) {
			receipt.Source.SizeBytes = maxJSONSafeInteger + 1
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := base
			mutate(&receipt)
			if err := ValidateProgramReceipt(receipt); err == nil {
				t.Fatal("ValidateProgramReceipt returned nil error")
			}
			if _, err := CanonicalProgramReceipt(receipt); err == nil {
				t.Fatal("CanonicalProgramReceipt returned nil error")
			}
		})
	}
}

func TestProgramReceiptParserRequiresClosedCanonicalObject(t *testing.T) {
	canonical, err := CanonicalProgramReceipt(testProgramReceipt(t))
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string][]byte{
		"noncanonical": append([]byte(" "), canonical...),
		"unknown":      append(canonical[:len(canonical)-1], []byte(`,"unknown":true}`)...),
		"duplicate": []byte(strings.Replace(
			string(canonical),
			`"formatVersion":0`,
			`"formatVersion":0,"formatVersion":0`,
			1,
		)),
		"empty": nil,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProgramReceipt(raw); err == nil {
				t.Fatal("ParseProgramReceipt returned nil error")
			}
		})
	}
}

func TestProgramReceiptCanonicalSizeBound(t *testing.T) {
	if _, err := ParseProgramReceipt(make([]byte, maxProgramReceiptSizeBytes+1)); err == nil {
		t.Fatal("ParseProgramReceipt accepted oversized input")
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

func testProgramReceipt(t *testing.T) ProgramReceipt {
	t.Helper()
	receipt, err := NewProgramReceipt(
		testProgramIndex(t),
		testBuildProvenance(t),
		testCompilerInputs(),
		"24.16.0",
		testProgramBuildManifest(t),
		"sha256:"+strings.Repeat("8", 64),
		ProgramReceiptSource{
			Digest:    "sha256:" + strings.Repeat("5", 64),
			MediaType: "application/vnd.helmr.deployment-source.v0+tar",
			SizeBytes: 1,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
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
