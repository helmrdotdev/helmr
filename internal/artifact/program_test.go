package artifact

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/definition"
)

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
		parsed.Definitions[2].Kind != definition.KindTemplate {
		t.Fatalf("Program index declarations are not in unsigned UTF-8 kind order")
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
		{name: "absent Computer", change: func(index *ProgramMetadata) { index.Definitions[0].Agent.TemplateDefinitionID = "absent" }},
		{name: "wrong manifest kind", change: func(index *ProgramMetadata) { index.Definitions[0].Template = index.Definitions[2].Template }},
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

func TestPayloadDigestMatchesFixedVector(t *testing.T) {
	files := map[string]string{
		"helmr/app/main.mjs": "export const value = 42;\n",
		"package.json":       "{\"type\":\"module\"}\n",
	}
	// Deliberately unordered; metadata is excluded while application files,
	// directory modes and symbolic-link targets contribute to the payload.
	entries := []Entry{
		{Path: "package.json", Kind: EntryRegular, Mode: 0o644, SizeBytes: 18},
		{Path: "helmr/config.json", Kind: EntryRegular, Mode: 0o644, SizeBytes: 2},
		{Path: "main.mjs", Kind: EntrySymlink, Mode: 0o777, LinkTarget: "helmr/app/main.mjs"},
		{Path: "helmr/app/main.mjs", Kind: EntryRegular, Mode: 0o644, SizeBytes: 25},
		{Path: "helmr", Kind: EntryDirectory, Mode: 0o755},
		{Path: "helmr/app", Kind: EntryDirectory, Mode: 0o755},
	}
	digest, err := PayloadDigest(t.Context(), entries, func(_ context.Context, path string) (io.ReadCloser, error) {
		content, ok := files[path]
		if !ok {
			t.Fatalf("unexpected payload read: %s", path)
		}
		return io.NopCloser(strings.NewReader(content)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Fixed SHA-256 of the payload domain and canonical entry records, derived
	// independently of PayloadDigest and the production canonicalizer.
	const want = "sha256:065015dd2e502284542698c859fe2b828745d2a311f4bf7a0562204c52026ac4"
	if digest != want {
		t.Fatalf("payload digest = %s, want %s", digest, want)
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
