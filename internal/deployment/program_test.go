package deployment

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type contractFixture struct {
	Manifest struct {
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		DigestHex string `json:"digestHex"`
	} `json:"manifest"`
}

func TestProgramIndexCanonicalRoundTrip(t *testing.T) {
	index := testProgramIndex(t)
	raw, err := CanonicalProgramIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramIndex(raw)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := CanonicalProgramIndex(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(raw) {
		t.Fatalf("reencoded Program index differs:\n%s\n%s", reencoded, raw)
	}
	if parsed.Declarations[0].Kind != DefinitionKindActor ||
		parsed.Declarations[1].Kind != DefinitionKindSandbox ||
		parsed.Declarations[2].Kind != DefinitionKindTask {
		t.Fatalf("Program index declarations are not in unsigned UTF-8 kind order")
	}
}

func TestProgramOutputCanonicalRoundTrip(t *testing.T) {
	output := ProgramOutput{
		Artifact: ProgramDescriptor{
			Digest:    "sha256:" + strings.Repeat("a", 64),
			SizeBytes: 1024,
			MediaType: ProgramArtifactMediaType,
		},
		Index: testProgramIndex(t),
	}
	raw, err := CanonicalProgramOutput(output)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramOutput(raw)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := CanonicalProgramOutput(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(reencoded) {
		t.Fatalf("Program output changed:\n%s\n%s", raw, reencoded)
	}
}

func TestProgramIndexRejectsInvalidAuthority(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ProgramIndex)
	}{
		{
			name: "empty declarations",
			change: func(index *ProgramIndex) {
				index.Declarations = nil
			},
		},
		{
			name: "declaration order",
			change: func(index *ProgramIndex) {
				index.Declarations[0], index.Declarations[1] =
					index.Declarations[1], index.Declarations[0]
			},
		},
		{
			name: "duplicate queue",
			change: func(index *ProgramIndex) {
				index.Queues = append(index.Queues, index.Queues[0])
			},
		},
		{
			name: "invalid runtime API",
			change: func(index *ProgramIndex) {
				index.RuntimeContract = "helmr.runtime.v1"
			},
		},
		{
			name: "invalid config digest",
			change: func(index *ProgramIndex) {
				index.ConfigResultDigest = "sha256:invalid"
			},
		},
		{
			name: "invalid locator",
			change: func(index *ProgramIndex) {
				index.Declarations[0].Locator.ModulePath = "tasks/operator.ts"
			},
		},
		{
			name: "Sandbox locator",
			change: func(index *ProgramIndex) {
				index.Declarations[1].Locator = &ProgramLocator{
					ExportName: "repo",
					ModulePath: ".helmr/modules/" + strings.Repeat("a", 64) + ".mjs",
					Slot:       DeclarationSlotHandler,
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index := cloneProgramIndex(testProgramIndex(t))
			test.change(&index)
			if err := ValidateProgramIndex(index); err == nil {
				t.Fatal("ValidateProgramIndex returned nil error")
			}
		})
	}
}

func TestProgramIndexRejectsUnknownAndNoncanonicalJSON(t *testing.T) {
	raw, err := CanonicalProgramIndex(testProgramIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	value["unknown"] = true
	unknown, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseProgramIndex(unknown); err == nil {
		t.Fatal("ParseProgramIndex accepted an unknown root member")
	}
	if _, err := ParseProgramIndex(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseProgramIndex accepted noncanonical bytes")
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

func testProgramIndex(t *testing.T) ProgramIndex {
	t.Helper()
	plan := testBuildPlan()
	index, err := buildProgramIndex(
		plan,
		testAnalysisDeclarationLocator(),
		[]BundleWorkspaceImage{{
			DeclaredID: "repo",
			Artifact: BundleWorkspaceImageArtifact{
				Digest:       "sha256:" + strings.Repeat("d", 64),
				SizeBytes:    4096,
				MediaType:    WorkspaceImageArtifactMediaType,
				Architecture: ArchitectureX8664,
			},
		}},
		"sha256:"+strings.Repeat("4", 64),
		"sha256:"+strings.Repeat("f", 64),
	)
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func loadContractFixture(t *testing.T) contractFixture {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	raw, err := os.ReadFile(filepath.Join(
		filepath.Dir(source),
		"..",
		"..",
		"fixtures",
		"contracts",
		"deployment-v0",
		"golden.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var fixture contractFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}
