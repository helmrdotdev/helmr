package deployment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestProgramManifestRoundTrip(t *testing.T) {
	manifest := testProgramManifest(t)
	canonical, err := canonicalProgramManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseProgramManifest(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatalf("parsed manifest = %#v, want %#v", parsed, manifest)
	}
}

func TestProgramManifestRejectsProducerAuthority(t *testing.T) {
	manifest := testProgramManifest(t)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	producerFields := map[string]any{
		"compiler":       map[string]any{},
		"configSource":   map[string]any{},
		"inputs":         []any{},
		"lockfile":       map[string]any{},
		"packageManager": "bun@1.3.10",
	}
	for name, value := range producerFields {
		t.Run(name, func(t *testing.T) {
			candidate := make(map[string]any, len(document)+1)
			for key, value := range document {
				candidate[key] = value
			}
			candidate[name] = value
			encoded, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := jsoncanon.Transform(encoded)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseProgramManifest(canonical); err == nil {
				t.Fatalf("ParseProgramManifest accepted producer field %q", name)
			}
		})
	}
}

func TestProgramManifestRejectsInvalidFinalAuthority(t *testing.T) {
	manifest := testProgramManifest(t)
	tests := map[string]func(*ProgramManifest){
		"format": func(value *ProgramManifest) {
			value.FormatVersion++
		},
		"config": func(value *ProgramManifest) {
			value.Config.Digest = "invalid"
		},
		"index": func(value *ProgramManifest) {
			value.ProgramIndexDigest = "invalid"
		},
		"module": func(value *ProgramManifest) {
			value.Modules[0].ModuleDigest = "invalid"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := manifest
			value.Modules = append([]ProgramModule(nil), manifest.Modules...)
			mutate(&value)
			if err := validateProgramManifest(value); err == nil {
				t.Fatal("validateProgramManifest returned nil error")
			}
		})
	}
}

func testProgramManifest(t *testing.T) ProgramManifest {
	t.Helper()
	indexRaw, err := CanonicalProgramIndex(testProgramIndex(t))
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := "tasks/build.ts"
	modulePath := generatedDeclarationModulePath(sourcePath)
	return ProgramManifest{
		FormatVersion: ProgramManifestFormatVersion,
		Config: ProgramPathDigest{
			Digest: "sha256:" + strings.Repeat("4", 64),
			Path:   "helmr/config.json",
		},
		ExternalEdges: []ProgramExternalEdge{},
		LocalPackages: []ProgramLocalPackage{},
		Modules: []ProgramModule{{
			ModuleDigest:    "sha256:" + strings.Repeat("b", 64),
			ModulePath:      modulePath,
			SourceMapDigest: "sha256:" + strings.Repeat("c", 64),
			SourceMapPath:   modulePath + ".map",
			SourcePath:      sourcePath,
		}},
		ProgramIndexDigest: testDigest(string(indexRaw)),
	}
}
