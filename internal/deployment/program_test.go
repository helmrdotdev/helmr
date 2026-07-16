package deployment

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type contractFixture struct {
	ProgramIndex struct {
		Canonical string `json:"canonical"`
	} `json:"programIndex"`
	ProgramRejections []struct {
		Name     string `json:"name"`
		Mutation string `json:"mutation"`
	} `json:"programRejections"`
	RuntimeABI struct {
		Canonical string `json:"canonical"`
		DigestHex string `json:"digestHex"`
	} `json:"runtimeAbi"`
	Manifest struct {
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		DigestHex string `json:"digestHex"`
	} `json:"manifest"`
}

func TestProgramIndexMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	index, err := ParseProgramIndex([]byte(fixture.ProgramIndex.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.ProgramIndex.Canonical {
		t.Fatalf("canonical program index = %q, want %q", canonical, fixture.ProgramIndex.Canonical)
	}
	gotIDs := make([]string, 0, 6)
	for _, declaration := range index.Declarations {
		if declaration.Kind == DeclarationKindTask {
			gotIDs = append(gotIDs, declaration.DeclaredID)
		}
	}
	wantIDs := []string{"Build-", "Build.", "Build0", "BuildA", "Build_", "Builda"}
	if !slices.Equal(gotIDs, wantIDs) {
		t.Fatalf("task declaration order = %v, want %v", gotIDs, wantIDs)
	}
}

func TestProgramIndexRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	base, err := ParseProgramIndex([]byte(fixture.ProgramIndex.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range fixture.ProgramRejections {
		t.Run(test.Name, func(t *testing.T) {
			index := cloneProgramIndex(base)
			switch test.Mutation {
			case "empty_declarations":
				index.Declarations = nil
			case "missing_format_version":
				var value map[string]any
				if err := json.Unmarshal([]byte(fixture.ProgramIndex.Canonical), &value); err != nil {
					t.Fatal(err)
				}
				delete(value, "formatVersion")
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				canonical, err := canonicalProgramInput(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ParseProgramIndex(canonical); err == nil {
					t.Fatal("ParseProgramIndex returned nil error")
				}
				return
			case "unknown_root_member":
				raw := []byte(fixture.ProgramIndex.Canonical[:len(fixture.ProgramIndex.Canonical)-1] + `,"unknown":true}`)
				canonical, err := canonicalProgramInput(raw)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := ParseProgramIndex(canonical); err == nil {
					t.Fatal("ParseProgramIndex returned nil error")
				}
				return
			case "declaration_order":
				index.Declarations[0], index.Declarations[1] = index.Declarations[1], index.Declarations[0]
			case "task_slots":
				index.Declarations[0].Slots = []DeclarationSlot{DeclarationSlotPayloadSchema, DeclarationSlotHandler}
			case "duplicate_declaration":
				index.Declarations[1] = index.Declarations[0]
			case "runtime_abi":
				index.RuntimeContract.RuntimeAPIVersion = "helmr.runtime-api.v2"
			default:
				t.Fatalf("unknown fixture mutation %q", test.Mutation)
			}
			if err := ValidateProgramIndex(index); err == nil {
				t.Fatal("ValidateProgramIndex returned nil error")
			}
		})
	}
}

func TestProgramIndexParserRequiresCanonicalBytes(t *testing.T) {
	fixture := loadContractFixture(t)
	if _, err := ParseProgramIndex([]byte(" " + fixture.ProgramIndex.Canonical)); err == nil {
		t.Fatal("ParseProgramIndex returned nil error")
	}
}

func TestRuntimeABIRejectsEverySingleFieldMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProgramRuntimeABI)
	}{
		{name: "bundle format", mutate: func(abi *ProgramRuntimeABI) { abi.BundleFormatVersion = "helmr.program-bundle.v2" }},
		{name: "runtime API", mutate: func(abi *ProgramRuntimeABI) { abi.RuntimeAPIVersion = "helmr.runtime-api.v2" }},
		{name: "checkpoint protocol", mutate: func(abi *ProgramRuntimeABI) { abi.CheckpointProtocolVersion = "helmr.checkpoint.v2" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			abi := CurrentProgramRuntimeABI()
			test.mutate(&abi)
			if err := ValidateCurrentProgramRuntimeABI(abi); err == nil {
				t.Fatal("ValidateCurrentProgramRuntimeABI returned nil error")
			}
		})
	}
}

func TestRuntimeABIAndManifestDigestsMatchSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	digest, err := ProgramRuntimeABIDigest(CurrentProgramRuntimeABI())
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(digest[:]) != fixture.RuntimeABI.DigestHex {
		t.Fatalf("runtime ABI digest = %x, want %s", digest, fixture.RuntimeABI.DigestHex)
	}
	abiRaw, err := json.Marshal(CurrentProgramRuntimeABI())
	if err != nil {
		t.Fatal(err)
	}
	abiCanonical, err := canonicalProgramInput(abiRaw)
	if err != nil {
		t.Fatal(err)
	}
	if string(abiCanonical) != fixture.RuntimeABI.Canonical {
		t.Fatalf("runtime ABI canonical JSON = %q, want %q", abiCanonical, fixture.RuntimeABI.Canonical)
	}

	canonical, manifestDigest, err := CanonicalManifestAndDigest([]byte(fixture.Manifest.Input))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.Manifest.Canonical {
		t.Fatalf("manifest canonical JSON = %q, want %q", canonical, fixture.Manifest.Canonical)
	}
	if hex.EncodeToString(manifestDigest[:]) != fixture.Manifest.DigestHex {
		t.Fatalf("manifest digest = %x, want %s", manifestDigest, fixture.Manifest.DigestHex)
	}
}

func cloneProgramIndex(index ProgramIndex) ProgramIndex {
	cloned := index
	cloned.SupportedArchitectures = slices.Clone(index.SupportedArchitectures)
	cloned.Declarations = make([]ProgramDeclaration, len(index.Declarations))
	for position, declaration := range index.Declarations {
		cloned.Declarations[position] = declaration
		cloned.Declarations[position].Slots = slices.Clone(declaration.Slots)
	}
	return cloned
}

func canonicalProgramInput(raw []byte) ([]byte, error) {
	return jsoncanon.Transform(raw)
}

func loadContractFixture(t *testing.T) contractFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(source), "..", "..", "fixtures", "contracts", "deployment-v0", "golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture contractFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
