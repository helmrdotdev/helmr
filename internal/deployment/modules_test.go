package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestModuleMapMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	moduleMap, err := ParseModuleMap([]byte(fixture.ModuleMap.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalModuleMap(moduleMap)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.ModuleMap.Canonical {
		t.Fatalf("canonical module map = %q, want %q", canonical, fixture.ModuleMap.Canonical)
	}
	if moduleMap.Modules[2].Path != "packages/shared/src/\ue000.ts" || moduleMap.Modules[3].Path != "packages/shared/src/😀.ts" {
		t.Fatalf("module map does not use unsigned UTF-8 path order: %q, %q", moduleMap.Modules[2].Path, moduleMap.Modules[3].Path)
	}
}

func TestModuleMapRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.ModuleMapRejections {
		t.Run(test.Name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal([]byte(fixture.ModuleMap.Canonical), &value); err != nil {
				t.Fatal(err)
			}
			modules := value["modules"].([]any)
			first := modules[0].(map[string]any)
			switch test.Mutation {
			case "missing_format_version":
				delete(value, "formatVersion")
			case "unknown_root_member":
				value["unknown"] = true
			case "transformer":
				value["transformer"] = "helmr.typescript.v1"
			case "module_order":
				modules[0], modules[1] = modules[1], modules[0]
			case "duplicate_path":
				modules[1] = first
			case "absolute_path":
				setFixtureModulePath(first, "/packages/shared/src/legacy.cts")
			case "escaping_path":
				setFixtureModulePath(first, "packages/../src/legacy.cts")
			case "backslash_path":
				setFixtureModulePath(first, `packages\shared\src\legacy.cts`)
			case "reserved_helmr_root":
				setFixtureModulePath(first, "helmr/legacy.cts")
			case "reserved_dot_helmr_root":
				setFixtureModulePath(first, ".helmr/legacy.cts")
			case "reserved_node_modules_root":
				setFixtureModulePath(first, "node_modules/legacy.cts")
			case "declaration_path":
				setFixtureModulePath(first, "packages/shared/src/legacy.d.cts")
			case "unsupported_extension":
				setFixtureModulePath(first, "packages/shared/src/legacy.tsx")
			case "source_digest":
				first["sourceDigest"] = "sha256:invalid"
			case "code_digest":
				first["codeDigest"] = "sha256:invalid"
			case "format":
				first["format"] = "module"
				setFixtureModulePath(first, first["path"].(string))
			case "code_path_key":
				first["codePath"] = "helmr/files/modules/" + strings.Repeat("0", 64) + ".cjs"
			case "code_path_extension":
				first["codePath"] = strings.TrimSuffix(first["codePath"].(string), ".cjs") + ".mjs"
			case "unknown_module_member":
				first["unknown"] = true
			default:
				t.Fatalf("unknown fixture mutation %q", test.Mutation)
			}
			raw, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			canonical, err := jsoncanon.Transform(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseModuleMap(canonical); err == nil {
				t.Fatal("ParseModuleMap returned nil error")
			}
		})
	}
}

func setFixtureModulePath(module map[string]any, path string) {
	module["path"] = path
	module["codePath"] = moduleCodePath(path, ModuleFormat(module["format"].(string)))
}

func TestModuleMapAcceptsEmptyModuleArray(t *testing.T) {
	moduleMap := ModuleMap{
		FormatVersion: ModuleMapFormatVersion,
		Modules:       []Module{},
		Transformer:   TypeScriptTransformer,
	}
	canonical, err := CanonicalModuleMap(moduleMap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseModuleMap(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestModuleMapRejectsNonCanonicalAndBoundedInput(t *testing.T) {
	fixture := loadContractFixture(t)
	if _, err := ParseModuleMap([]byte(" " + fixture.ModuleMap.Canonical)); err == nil {
		t.Fatal("ParseModuleMap accepted non-canonical input")
	}
	if _, err := ParseModuleMap(nil); err == nil {
		t.Fatal("ParseModuleMap accepted empty input")
	}
	if _, err := ParseModuleMap(make([]byte, maxProgramFileSizeBytes+1)); err == nil {
		t.Fatal("ParseModuleMap accepted oversized input")
	}
}

func TestModuleMapRejectsTooManyModules(t *testing.T) {
	moduleMap := ModuleMap{
		FormatVersion: ModuleMapFormatVersion,
		Modules:       make([]Module, maxModuleCount+1),
		Transformer:   TypeScriptTransformer,
	}
	if err := ValidateModuleMap(moduleMap); err == nil {
		t.Fatal("ValidateModuleMap accepted too many modules")
	}
}
