package deployment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestDependencyIndexMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	index, err := ParseDependencyIndex([]byte(fixture.DependencyIndex.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalDependencyIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.DependencyIndex.Canonical {
		t.Fatalf("canonical dependency index = %q, want %q", canonical, fixture.DependencyIndex.Canonical)
	}
}

func TestDependencyIndexRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.DependencyIndexRejections {
		t.Run(test.Name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal([]byte(fixture.DependencyIndex.Canonical), &value); err != nil {
				t.Fatal(err)
			}
			manager := value["packageManager"].(map[string]any)
			lockfile := value["lockfile"].(map[string]any)
			switch test.Mutation {
			case "missing_format_version":
				delete(value, "formatVersion")
			case "unknown_root_member":
				value["unknown"] = true
			case "unknown_manager_member":
				manager["unknown"] = true
			case "unknown_lockfile_member":
				lockfile["unknown"] = true
			case "runtime_api_member":
				value["runtimeApiVersion"] = RuntimeAPIVersion
			case "manager_name":
				manager["name"] = "pnpm"
			case "manager_version_leading_v":
				manager["version"] = "v1.3.10"
			case "manager_version_range":
				manager["version"] = "^1.3.10"
			case "manager_version_build":
				manager["version"] = "1.3.10+build"
			case "manager_version_leading_zero":
				manager["version"] = "01.3.10"
			case "manager_version_prerelease_zero":
				manager["version"] = "1.3.10-01"
			case "manager_version_newline":
				manager["version"] = "1.3.10\n"
			case "manager_version_oversize":
				manager["version"] = "1.2.3-" + strings.Repeat("a", 59)
			case "lockfile_name":
				lockfile["name"] = "package-lock.json"
			case "lockfile_digest":
				lockfile["digest"] = "sha256:invalid"
			case "local_manifests_digest":
				value["localManifestsDigest"] = "sha256:invalid"
			case "dependency_tools_digest":
				value["dependencyToolsDigest"] = "sha256:invalid"
			case "package_graph_digest":
				value["packageGraphDigest"] = "sha256:invalid"
			case "package_graph_size_zero":
				value["packageGraphSizeBytes"] = 0
			case "package_graph_size_fractional":
				value["packageGraphSizeBytes"] = 1.5
			case "package_graph_size_oversize":
				value["packageGraphSizeBytes"] = maxProgramFileSizeBytes + 1
			case "materializer_version":
				value["materializerVersion"] = "helmr.dependencies.v1"
			case "runtime_digest":
				value["runtimeDigest"] = "sha256:invalid"
			case "architecture":
				value["architecture"] = "amd64"
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
			if _, err := ParseDependencyIndex(canonical); err == nil {
				t.Fatal("ParseDependencyIndex returned nil error")
			}
		})
	}
}

func TestDependencyIndexRejectsMissingAndNullMembers(t *testing.T) {
	fixture := loadContractFixture(t)
	var base map[string]any
	if err := json.Unmarshal([]byte(fixture.DependencyIndex.Canonical), &base); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{"architecture", "dependencyToolsDigest", "formatVersion", "localManifestsDigest", "lockfile", "materializerVersion", "packageGraphDigest", "packageGraphSizeBytes", "packageManager", "runtimeDigest"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(mode+"_root_"+member, func(t *testing.T) {
				value := cloneJSONMap(t, base)
				if mode == "missing" {
					delete(value, member)
				} else {
					value[member] = nil
				}
				requireDependencyIndexRejection(t, value)
			})
		}
	}
	for _, objectName := range []string{"packageManager", "lockfile"} {
		for _, member := range []string{"name", map[string]string{"packageManager": "version", "lockfile": "digest"}[objectName]} {
			for _, mode := range []string{"missing", "null"} {
				t.Run(mode+"_"+objectName+"_"+member, func(t *testing.T) {
					value := cloneJSONMap(t, base)
					object := value[objectName].(map[string]any)
					if mode == "missing" {
						delete(object, member)
					} else {
						object[member] = nil
					}
					requireDependencyIndexRejection(t, value)
				})
			}
		}
	}
}

func TestDependencyIndexRejectsSharedDuplicateMembers(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.DependencyIndexRawRejections {
		t.Run(test.Name, func(t *testing.T) {
			raw := fixture.DependencyIndex.Canonical
			switch test.Mutation {
			case "duplicate_root_member":
				raw = strings.Replace(raw, `"formatVersion":0`, `"formatVersion":0,"formatVersion":0`, 1)
			case "duplicate_manager_member":
				raw = strings.Replace(raw, `"packageManager":{"name":"bun"`, `"packageManager":{"name":"bun","name":"bun"`, 1)
			case "duplicate_lockfile_member":
				raw = strings.Replace(raw, `"lockfile":{"digest":`, `"lockfile":{"digest":"sha256:`+strings.Repeat("1", 64)+`","digest":`, 1)
			default:
				t.Fatalf("unknown raw fixture mutation %q", test.Mutation)
			}
			if _, err := ParseDependencyIndex([]byte(raw)); err == nil || !strings.Contains(err.Error(), "duplicate object name") {
				t.Fatalf("ParseDependencyIndex error = %v, want duplicate object name", err)
			}
		})
	}
}

func TestDependencyIndexAcceptsManagerPairsAndPrerelease(t *testing.T) {
	fixture := loadContractFixture(t)
	index, err := ParseDependencyIndex([]byte(fixture.DependencyIndex.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	index.PackageManager.Version = "1.3.10-rc.1"
	if _, err := CanonicalDependencyIndex(index); err != nil {
		t.Fatal(err)
	}
	index.PackageManager.Version = "1.2.3-" + strings.Repeat("a", 58)
	if _, err := CanonicalDependencyIndex(index); err != nil {
		t.Fatal(err)
	}
	index.PackageManager = PackageManager{Name: PackageManagerNPM, Version: "10.9.4"}
	index.Lockfile.Name = "package-lock.json"
	canonical, err := CanonicalDependencyIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDependencyIndex(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestDependencyIndexRejectsNonCanonicalAndBoundedInput(t *testing.T) {
	fixture := loadContractFixture(t)
	if _, err := ParseDependencyIndex([]byte(" " + fixture.DependencyIndex.Canonical)); err == nil {
		t.Fatal("ParseDependencyIndex accepted non-canonical input")
	}
	if _, err := ParseDependencyIndex(nil); err == nil {
		t.Fatal("ParseDependencyIndex accepted empty input")
	}
	if _, err := ParseDependencyIndex(make([]byte, maxDependencyIndexSizeBytes+1)); err == nil {
		t.Fatal("ParseDependencyIndex accepted oversized input")
	}
}

func TestDependencyCacheInputMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	var input DependencyCacheInput
	if err := json.Unmarshal([]byte(fixture.DependencyCacheInput.Canonical), &input); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalDependencyCacheInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.DependencyCacheInput.Canonical {
		t.Fatalf("canonical dependency cache input = %q, want %q", canonical, fixture.DependencyCacheInput.Canonical)
	}
	key, err := DependencyCacheKey(input)
	if err != nil {
		t.Fatal(err)
	}
	if key != fixture.DependencyCacheInput.Key {
		t.Fatalf("dependency cache key = %q, want %q", key, fixture.DependencyCacheInput.Key)
	}
}

func TestDependencyCacheInputRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.DependencyCacheInputRejections {
		t.Run(test.Name, func(t *testing.T) {
			var input DependencyCacheInput
			if err := json.Unmarshal([]byte(fixture.DependencyCacheInput.Canonical), &input); err != nil {
				t.Fatal(err)
			}
			switch test.Mutation {
			case "invalid_format_version":
				input.FormatVersion = 1
			case "invalid_manager":
				input.PackageManager.Name = PackageManagerName("pnpm")
			case "invalid_manager_version":
				input.PackageManager.Version = "^1.3.10"
			case "mismatched_lockfile":
				input.Lockfile.Name = "package-lock.json"
			case "invalid_lockfile_digest":
				input.Lockfile.Digest = "sha256:invalid"
			case "invalid_local_manifests_digest":
				input.LocalManifestsDigest = "sha256:invalid"
			case "invalid_dependency_tools_digest":
				input.DependencyToolsDigest = "sha256:invalid"
			case "invalid_materializer_version":
				input.MaterializerVersion = "helmr.dependencies.v1"
			case "invalid_runtime_digest":
				input.RuntimeDigest = "sha256:invalid"
			case "invalid_architecture":
				input.Architecture = RuntimeArchitecture("amd64")
			default:
				t.Fatalf("unknown fixture mutation %q", test.Mutation)
			}
			if _, err := DependencyCacheKey(input); err == nil {
				t.Fatal("DependencyCacheKey returned nil error")
			}
		})
	}
}

func TestDependencyCacheKeyBindsEveryInput(t *testing.T) {
	fixture := loadContractFixture(t)
	var base DependencyCacheInput
	if err := json.Unmarshal([]byte(fixture.DependencyCacheInput.Canonical), &base); err != nil {
		t.Fatal(err)
	}
	baseKey, err := DependencyCacheKey(base)
	if err != nil {
		t.Fatal(err)
	}
	tests := map[string]func(*DependencyCacheInput){
		"architecture": func(input *DependencyCacheInput) {
			input.Architecture = ArchitectureAArch64
		},
		"dependency tools": func(input *DependencyCacheInput) {
			input.DependencyToolsDigest = "sha256:" + strings.Repeat("8", 64)
		},
		"local manifests": func(input *DependencyCacheInput) {
			input.LocalManifestsDigest = "sha256:" + strings.Repeat("5", 64)
		},
		"lockfile": func(input *DependencyCacheInput) {
			input.Lockfile.Digest = "sha256:" + strings.Repeat("6", 64)
		},
		"manager": func(input *DependencyCacheInput) {
			input.PackageManager.Version = "1.3.11"
		},
		"runtime": func(input *DependencyCacheInput) {
			input.RuntimeDigest = "sha256:" + strings.Repeat("7", 64)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			key, err := DependencyCacheKey(input)
			if err != nil {
				t.Fatal(err)
			}
			if key == baseKey {
				t.Fatalf("dependency cache key did not bind %s", name)
			}
		})
	}
}

func cloneJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var cloned map[string]any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func requireDependencyIndexRejection(t *testing.T, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseDependencyIndex(canonical); err == nil {
		t.Fatal("ParseDependencyIndex returned nil error")
	}
}
