package artifact

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/definition"
)

type contractFixture struct {
	Manifest struct {
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		DigestHex string `json:"digestHex"`
	} `json:"manifest"`
}

func TestProgramMetadataCanonicalRoundTrip(t *testing.T) {
	index := testProgramMetadata(t)
	raw, err := CanonicalProgramMetadata(index)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramMetadata(raw)
	if err != nil {
		t.Fatal(err)
	}
	reencoded, err := CanonicalProgramMetadata(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if string(reencoded) != string(raw) {
		t.Fatalf("reencoded Program index differs:\n%s\n%s", reencoded, raw)
	}
	if parsed.Definitions[0].Kind != definition.KindAgent ||
		parsed.Definitions[1].Kind != definition.KindAgent ||
		parsed.Definitions[2].Kind != definition.KindComputer {
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
		Metadata: testProgramMetadata(t),
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

func TestProgramMetadataRejectsInvalidAuthority(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ProgramMetadata)
	}{
		{
			name: "empty declarations",
			change: func(index *ProgramMetadata) {
				index.Definitions = nil
			},
		},
		{
			name: "declaration order",
			change: func(index *ProgramMetadata) {
				index.Definitions[0], index.Definitions[1] =
					index.Definitions[1], index.Definitions[0]
			},
		},
		{
			name: "invalid runtime API",
			change: func(index *ProgramMetadata) {
				index.RuntimeContract = "helmr.runtime.unsupported"
			},
		},
		{
			name: "invalid config digest",
			change: func(index *ProgramMetadata) {
				index.ConfigResultDigest = "sha256:invalid"
			},
		},
		{name: "absent Computer", change: func(index *ProgramMetadata) { index.Definitions[0].Agent.ComputerDefinitionID = "absent" }},
		{name: "wrong manifest kind", change: func(index *ProgramMetadata) { index.Definitions[0].Computer = index.Definitions[2].Computer }},
		{name: "duplicate declaration", change: func(index *ProgramMetadata) { index.Definitions[1] = index.Definitions[0] }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			index := cloneProgramMetadata(testProgramMetadata(t))
			test.change(&index)
			if err := ValidateProgramMetadata(index); err == nil {
				t.Fatal("ValidateProgramMetadata returned nil error")
			}
		})
	}
}

func TestProgramMetadataRejectsUnknownAndNoncanonicalJSON(t *testing.T) {
	raw, err := CanonicalProgramMetadata(testProgramMetadata(t))
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
	if _, err := ParseProgramMetadata(unknown); err == nil {
		t.Fatal("ParseProgramMetadata accepted an unknown root member")
	}
	if _, err := ParseProgramMetadata(append([]byte(" "), raw...)); err == nil {
		t.Fatal("ParseProgramMetadata accepted noncanonical bytes")
	}
}

func TestProgramMetadataParserEnforcesSizeBound(t *testing.T) {
	if _, err := ParseProgramMetadata(nil); err == nil {
		t.Fatal("ParseProgramMetadata accepted empty input")
	}
	if _, err := ParseProgramMetadata(make([]byte, MaxProgramFileSizeBytes+1)); err == nil {
		t.Fatal("ParseProgramMetadata accepted oversized input")
	}
}

func TestManifestDigestMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	canonical, manifestDigest, err := definition.CanonicalManifestAndDigest([]byte(fixture.Manifest.Input))
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

func testProgramMetadata(t *testing.T) ProgramMetadata {
	t.Helper()
	plan := testBuildPlan()
	index, err := BuildProgramMetadata(
		plan,
		testDefinitionIndex(),
		map[string]definition.ComputerSeed{
			"repo": {
				Profile:      definition.ComputerSeedProfile,
				Digest:       "sha256:" + strings.Repeat("d", 64),
				SizeBytes:    4096,
				MediaType:    definition.ComputerSeedMediaType,
				Architecture: definition.ArchitectureX8664,
			},
		},
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
		"tests",
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
