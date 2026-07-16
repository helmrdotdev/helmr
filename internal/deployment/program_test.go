package deployment

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	ModuleMap struct {
		Canonical string `json:"canonical"`
	} `json:"moduleMap"`
	ModuleMapRejections []struct {
		Name     string `json:"name"`
		Mutation string `json:"mutation"`
	} `json:"moduleMapRejections"`
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
			fractionalField, fractional := map[string]string{
				"dependency_size_fractional":    "dependencies",
				"package_graph_size_fractional": "packageGraph",
				"modules_size_fractional":       "modules",
			}[test.Mutation]
			if fractional {
				var value map[string]any
				if err := json.Unmarshal([]byte(fixture.ProgramIndex.Canonical), &value); err != nil {
					t.Fatal(err)
				}
				value[fractionalField].(map[string]any)["sizeBytes"] = 1.5
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
			}

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
			case "runtime_api":
				index.RuntimeAPIVersion = "helmr.runtime.v1"
			case "runtime_digest":
				index.RuntimeDigest = "sha256:" + strings.Repeat("A", 64)
			case "dependency_digest":
				index.Dependencies.Digest = "sha256:invalid"
			case "dependency_size_zero":
				index.Dependencies.SizeBytes = 0
			case "dependency_size_negative":
				index.Dependencies.SizeBytes = -1
			case "dependency_size_unsafe":
				index.Dependencies.SizeBytes = 9007199254740992
			case "dependency_media_type":
				index.Dependencies.MediaType += "; charset=binary"
			case "package_graph_digest":
				index.PackageGraph.Digest = "sha256:invalid"
			case "package_graph_size_zero":
				index.PackageGraph.SizeBytes = 0
			case "package_graph_size_oversize":
				index.PackageGraph.SizeBytes = maxProgramFileSizeBytes + 1
			case "modules_digest":
				index.Modules.Digest = "sha256:invalid"
			case "modules_size_zero":
				index.Modules.SizeBytes = 0
			case "modules_size_oversize":
				index.Modules.SizeBytes = maxProgramFileSizeBytes + 1
			case "architecture":
				index.Architecture = "amd64"
			case "declared_id":
				index.Declarations[0].DeclaredID = "invalid/id"
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

func TestProgramIndexAcceptsFileSizeBounds(t *testing.T) {
	fixture := loadContractFixture(t)
	index, err := ParseProgramIndex([]byte(fixture.ProgramIndex.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	index.PackageGraph.SizeBytes = 1
	index.Modules.SizeBytes = maxProgramFileSizeBytes
	canonical, err := CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProgramIndex(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestManifestDigestMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
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
