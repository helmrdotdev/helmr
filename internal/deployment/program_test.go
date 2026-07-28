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
			case "build_contract":
				index.BuildContractVersion = "helmr.program-build.v1"
			case "runtime_api":
				index.RuntimeAPIVersion = "helmr.runtime.v1"
			case "runtime_digest":
				index.RuntimeDigest = "sha256:" + strings.Repeat("A", 64)
			case "toolchain_digest":
				index.ToolchainDigest = "sha256:invalid"
			case "manager_name":
				index.Manager.Name = "pnpm"
			case "manager_version":
				index.Manager.Version = "^1.3.10"
			case "manager_digest":
				index.Manager.Digest = "sha256:invalid"
			case "lockfile_name":
				index.Submitted.LockfileName = "package-lock.json"
			case "lockfile_digest":
				index.Submitted.LockfileDigest = "sha256:invalid"
			case "source_digest":
				index.Submitted.SourceDigest = "sha256:invalid"
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

func TestProgramIndexAcceptsSupportedManagerLockfiles(t *testing.T) {
	fixture := loadContractFixture(t)
	tests := []struct {
		name     PackageManagerName
		version  string
		lockfile string
	}{
		{name: PackageManagerNPM, version: "11.4.2", lockfile: "package-lock.json"},
		{name: PackageManagerNPM, version: "11.4.2", lockfile: "npm-shrinkwrap.json"},
		{name: PackageManagerPNPM, version: "11.1.0", lockfile: "pnpm-lock.yaml"},
		{name: PackageManagerBun, version: "1.3.10", lockfile: "bun.lock"},
	}
	for _, test := range tests {
		t.Run(string(test.name)+"/"+test.lockfile, func(t *testing.T) {
			index, err := ParseProgramIndex([]byte(fixture.ProgramIndex.Canonical))
			if err != nil {
				t.Fatal(err)
			}
			index.Manager.Name = test.name
			index.Manager.Version = test.version
			index.Submitted.LockfileName = test.lockfile
			if err := ValidateProgramIndex(index); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgramIndexParserEnforcesSizeBound(t *testing.T) {
	if _, err := ParseProgramIndex(nil); err == nil {
		t.Fatal("ParseProgramIndex accepted empty input")
	}
	if _, err := ParseProgramIndex(make([]byte, maxProgramFileSizeBytes+1)); err == nil {
		t.Fatal("ParseProgramIndex accepted oversized input")
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
