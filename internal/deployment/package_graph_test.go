package deployment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func TestPackageGraphMatchesSharedGoldenFixture(t *testing.T) {
	fixture := loadContractFixture(t)
	graph, err := ParsePackageGraph([]byte(fixture.PackageGraph.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalPackageGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.PackageGraph.Canonical {
		t.Fatalf("canonical package graph = %q, want %q", canonical, fixture.PackageGraph.Canonical)
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != fixture.PackageGraph.DigestHex {
		t.Fatalf("package graph digest = %x, want %s", digest, fixture.PackageGraph.DigestHex)
	}
}

func TestPackageGraphRejectsSharedMutations(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, test := range fixture.PackageGraphRejections {
		t.Run(test.Name, func(t *testing.T) {
			value := packageGraphFixtureValue(t, fixture.PackageGraph.Canonical)
			locals := value["localPackages"].([]any)
			registries := value["registryPackages"].([]any)
			resolutions := value["resolutions"].([]any)
			root := locals[0].(map[string]any)
			local := locals[1].(map[string]any)
			registry := registries[0].(map[string]any)
			switch test.Mutation {
			case "missing_format_version":
				delete(value, "formatVersion")
			case "unknown_root_member":
				value["unknown"] = true
			case "unknown_local_member":
				local["unknown"] = true
			case "unknown_registry_member":
				registry["unknown"] = true
			case "unknown_resolution_member":
				resolutions[0].(map[string]any)["unknown"] = true
			case "unknown_endpoint_member":
				resolutions[0].(map[string]any)["from"].(map[string]any)["unknown"] = true
			case "root_not_first":
				locals[0], locals[1] = locals[1], locals[0]
			case "duplicate_root":
				value["localPackages"] = append(locals, cloneJSONValue(t, root))
			case "local_order":
				value["localPackages"] = append(locals, localFixture("packages/alpha", nil, nil))
			case "overlapping_local_path":
				value["localPackages"] = append(locals, localFixture("packages/shared/nested", nil, nil))
			case "non_adjacent_overlapping_local_path":
				value["localPackages"] = []any{
					locals[0],
					localFixture("a", nil, nil),
					localFixture("a-", nil, nil),
					localFixture("a/b", nil, nil),
					locals[1],
				}
			case "reserved_local_path":
				setFixtureLocalPath(t, value, "packages/shared", "helmr/shared")
			case "absolute_local_path":
				setFixtureLocalPath(t, value, "packages/shared", "/packages/shared")
			case "local_view_key":
				local["viewKey"] = strings.Repeat("0", 64)
			case "root_view_key":
				root["viewKey"] = strings.Repeat("0", 64)
			case "ambiguous_local_name":
				local["name"] = "app"
			case "registry_order":
				registries[0], registries[1] = registries[1], registries[0]
			case "duplicate_registry_path":
				registries[1].(map[string]any)["installPath"] = registry["installPath"]
			case "reserved_registry_path":
				registry["installPath"] = ".helmr/package"
			case "registry_integrity":
				registry["integrity"] = "sha512-invalid"
			case "registry_name":
				registry["name"] = "Invalid"
			case "empty_registry_version":
				registry["version"] = ""
			case "resolution_order":
				resolutions[0], resolutions[1] = resolutions[1], resolutions[0]
			case "resolution_relationship":
				resolutions[0].(map[string]any)["relationship"] = "development"
			case "resolution_dependency":
				resolutions[0].(map[string]any)["dependency"] = "Invalid"
			case "missing_from_node":
				resolutions[0].(map[string]any)["from"].(map[string]any)["path"] = "packages/missing"
			case "missing_to_node":
				resolutions[0].(map[string]any)["to"].(map[string]any)["installPath"] = "missing"
			case "unnamed_local_target":
				local["name"] = nil
			case "local_target_alias":
				resolutions[1].(map[string]any)["dependency"] = "alias"
			case "mixed_endpoint_shape":
				resolutions[0].(map[string]any)["from"].(map[string]any)["installPath"] = "zod"
			case "oversized_path_component":
				setFixtureLocalPath(t, value, "packages/shared", "packages/"+strings.Repeat("a", 256))
			case "oversized_mounted_path":
				setFixtureLocalPath(t, value, "packages/shared", pathWithLength(4096-len(programMountPath)-1))
			default:
				t.Fatalf("unknown fixture mutation %q", test.Mutation)
			}
			requirePackageGraphRejection(t, value)
		})
	}
}

func TestPackageGraphRejectsMissingNullAndDuplicateMembers(t *testing.T) {
	fixture := loadContractFixture(t)
	for _, member := range []string{"formatVersion", "localPackages", "registryPackages", "resolutions"} {
		for _, mode := range []string{"missing", "null"} {
			t.Run(mode+"_root_"+member, func(t *testing.T) {
				value := packageGraphFixtureValue(t, fixture.PackageGraph.Canonical)
				if mode == "missing" {
					delete(value, member)
				} else {
					value[member] = nil
				}
				requirePackageGraphRejection(t, value)
			})
		}
	}

	type memberSet struct {
		array    string
		position int
		members  []string
	}
	for _, object := range []memberSet{
		{array: "localPackages", position: 1, members: []string{"manifestDigest", "name", "path", "version", "viewKey"}},
		{array: "registryPackages", position: 0, members: []string{"installPath", "integrity", "name", "version"}},
		{array: "resolutions", position: 0, members: []string{"dependency", "from", "relationship", "to"}},
	} {
		for _, member := range object.members {
			t.Run("missing_"+object.array+"_"+member, func(t *testing.T) {
				value := packageGraphFixtureValue(t, fixture.PackageGraph.Canonical)
				entry := value[object.array].([]any)[object.position].(map[string]any)
				delete(entry, member)
				requirePackageGraphRejection(t, value)
			})
			if object.array != "localPackages" || (member != "name" && member != "version") {
				t.Run("null_"+object.array+"_"+member, func(t *testing.T) {
					value := packageGraphFixtureValue(t, fixture.PackageGraph.Canonical)
					entry := value[object.array].([]any)[object.position].(map[string]any)
					entry[member] = nil
					requirePackageGraphRejection(t, value)
				})
			}
		}
	}
	for _, endpointName := range []string{"from", "to"} {
		for _, member := range []string{"kind", "path"} {
			for _, mode := range []string{"missing", "null"} {
				t.Run(mode+"_"+endpointName+"_"+member, func(t *testing.T) {
					value := packageGraphFixtureValue(t, fixture.PackageGraph.Canonical)
					endpoint := value["resolutions"].([]any)[1].(map[string]any)[endpointName].(map[string]any)
					if mode == "missing" {
						delete(endpoint, member)
					} else {
						endpoint[member] = nil
					}
					requirePackageGraphRejection(t, value)
				})
			}
		}
	}

	for _, test := range fixture.PackageGraphRawRejections {
		t.Run(test.Name, func(t *testing.T) {
			raw := fixture.PackageGraph.Canonical
			switch test.Mutation {
			case "duplicate_root_member":
				raw = strings.Replace(raw, `"formatVersion":0`, `"formatVersion":0,"formatVersion":0`, 1)
			case "duplicate_local_member":
				raw = strings.Replace(raw, `"manifestDigest":"sha256:000`, `"manifestDigest":"sha256:`+strings.Repeat("0", 64)+`","manifestDigest":"sha256:000`, 1)
			case "duplicate_endpoint_member":
				raw = strings.Replace(raw, `"from":{"kind":"local"`, `"from":{"kind":"local","kind":"local"`, 1)
			default:
				t.Fatalf("unknown raw fixture mutation %q", test.Mutation)
			}
			if _, err := ParsePackageGraph([]byte(raw)); err == nil || !strings.Contains(err.Error(), "duplicate object name") {
				t.Fatalf("ParsePackageGraph error = %v, want duplicate object name", err)
			}
		})
	}
}

func TestPackageGraphAcceptsRootOnlyCyclesAndOpaqueVersions(t *testing.T) {
	fixture := loadContractFixture(t)
	rootOnly := PackageGraph{
		FormatVersion:    PackageGraphFormatVersion,
		LocalPackages:    []LocalPackage{{ManifestDigest: "sha256:" + strings.Repeat("0", 64), Path: "."}},
		RegistryPackages: []RegistryPackage{},
		Resolutions:      []PackageResolution{},
	}
	canonical, err := CanonicalPackageGraph(rootOnly)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.PackageGraph.RootOnlyCanonical {
		t.Fatalf("canonical root-only graph = %q, want %q", canonical, fixture.PackageGraph.RootOnlyCanonical)
	}
	digest := sha256.Sum256(canonical)
	if hex.EncodeToString(digest[:]) != fixture.PackageGraph.RootOnlyDigestHex {
		t.Fatalf("root-only graph digest = %x, want %s", digest, fixture.PackageGraph.RootOnlyDigestHex)
	}
	if _, err := ParsePackageGraph(canonical); err != nil {
		t.Fatal(err)
	}

	graph, err := ParsePackageGraph([]byte(fixture.PackageGraph.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	localVersion := "file:opaque"
	graph.LocalPackages[1].Version = &localVersion
	graph.RegistryPackages[0].Version = "link:opaque"
	root := graph.LocalPackages[0]
	local := graph.LocalPackages[1]
	graph.Resolutions = []PackageResolution{
		{
			Dependency:   *local.Name,
			From:         PackageEndpoint{Kind: PackageKindLocal, Path: &root.Path},
			Relationship: PackageRelationshipProduction,
			To:           PackageEndpoint{Kind: PackageKindLocal, Path: &local.Path},
		},
		{
			Dependency:   *root.Name,
			From:         PackageEndpoint{Kind: PackageKindLocal, Path: &local.Path},
			Relationship: PackageRelationshipProduction,
			To:           PackageEndpoint{Kind: PackageKindLocal, Path: &root.Path},
		},
	}
	if _, err := CanonicalPackageGraph(graph); err != nil {
		t.Fatal(err)
	}
}

func TestPackageGraphRejectsRegistryToLocalResolution(t *testing.T) {
	fixture := loadContractFixture(t)
	graph, err := ParsePackageGraph([]byte(fixture.PackageGraph.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	local := graph.LocalPackages[1]
	registry := graph.RegistryPackages[0]
	graph.Resolutions = []PackageResolution{{
		Dependency: *local.Name,
		From: PackageEndpoint{
			InstallPath: &registry.InstallPath,
			Kind:        PackageKindRegistry,
		},
		Relationship: PackageRelationshipProduction,
		To: PackageEndpoint{
			Kind: PackageKindLocal,
			Path: &local.Path,
		},
	}}
	if err := ValidatePackageGraph(graph); err == nil ||
		!strings.Contains(err.Error(), "registry-to-local resolution is unsupported") {
		t.Fatalf("ValidatePackageGraph error = %v", err)
	}
}

func TestPackageGraphUsesUnsignedUTF8PathOrderWithoutNormalization(t *testing.T) {
	paths := []string{"packages/e\u0301", "packages/é", "packages/\ue000", "packages/😀"}
	locals := []LocalPackage{{ManifestDigest: "sha256:" + strings.Repeat("0", 64), Path: "."}}
	for position, path := range paths {
		viewKey := localPackageViewKey(path)
		locals = append(locals, LocalPackage{
			ManifestDigest: "sha256:" + strings.Repeat(string(rune('1'+position)), 64),
			Path:           path,
			ViewKey:        &viewKey,
		})
	}
	graph := PackageGraph{
		FormatVersion:    PackageGraphFormatVersion,
		LocalPackages:    locals,
		RegistryPackages: []RegistryPackage{},
		Resolutions:      []PackageResolution{},
	}
	canonical, err := CanonicalPackageGraph(graph)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePackageGraph(canonical); err != nil {
		t.Fatal(err)
	}
}

func TestPackageGraphPathAndScalarBounds(t *testing.T) {
	if err := validatePackagePath(pathWithLength(4096-len(programMountPath)-2), programMountPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validatePackagePath(pathWithLength(4096-len(programMountPath)-1), programMountPath, true); err == nil {
		t.Fatal("validatePackagePath accepted a mounted path over 4096 bytes")
	}
	if err := validatePackagePath("packages/"+strings.Repeat("a", 255), programMountPath, true); err != nil {
		t.Fatal(err)
	}
	if err := validatePackagePath("packages/"+strings.Repeat("a", 256), programMountPath, true); err == nil {
		t.Fatal("validatePackagePath accepted a component over 255 bytes")
	}
	if err := validatePackageName(strings.Repeat("a", 214)); err != nil {
		t.Fatal(err)
	}
	if err := validatePackageName(strings.Repeat("a", 215)); err == nil {
		t.Fatal("validatePackageName accepted a name over 214 bytes")
	}
	if err := validatePackageVersion(strings.Repeat("v", 255)); err != nil {
		t.Fatal(err)
	}
	if err := validatePackageVersion(strings.Repeat("v", 256)); err == nil {
		t.Fatal("validatePackageVersion accepted a version over 255 bytes")
	}
}

func TestPackageGraphRejectsNonCanonicalAndBoundedInput(t *testing.T) {
	fixture := loadContractFixture(t)
	if _, err := ParsePackageGraph([]byte(" " + fixture.PackageGraph.Canonical)); err == nil {
		t.Fatal("ParsePackageGraph accepted non-canonical input")
	}
	if _, err := ParsePackageGraph(nil); err == nil {
		t.Fatal("ParsePackageGraph accepted empty input")
	}
	if _, err := ParsePackageGraph(make([]byte, maxProgramFileSizeBytes+1)); err == nil {
		t.Fatal("ParsePackageGraph accepted oversized input")
	}
}

func packageGraphFixtureValue(t *testing.T, canonical string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(canonical), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func requirePackageGraphRejection(t *testing.T, value map[string]any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParsePackageGraph(canonical); err == nil {
		t.Fatal("ParsePackageGraph returned nil error")
	}
}

func setFixtureLocalPath(t *testing.T, value map[string]any, oldPath, newPath string) {
	t.Helper()
	locals := value["localPackages"].([]any)
	for _, item := range locals {
		local := item.(map[string]any)
		if local["path"] == oldPath {
			local["path"] = newPath
			local["viewKey"] = localPackageViewKey(newPath)
		}
	}
	for _, item := range value["resolutions"].([]any) {
		resolution := item.(map[string]any)
		for _, endpointName := range []string{"from", "to"} {
			endpoint := resolution[endpointName].(map[string]any)
			if endpoint["kind"] == "local" && endpoint["path"] == oldPath {
				endpoint["path"] = newPath
			}
		}
	}
}

func localFixture(path string, name, version *string) map[string]any {
	return map[string]any{
		"manifestDigest": "sha256:" + strings.Repeat("2", 64),
		"name":           name,
		"path":           path,
		"version":        version,
		"viewKey":        localPackageViewKey(path),
	}
}

func cloneJSONValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var cloned any
	if err := json.Unmarshal(raw, &cloned); err != nil {
		t.Fatal(err)
	}
	return cloned
}

func pathWithLength(length int) string {
	var builder strings.Builder
	for builder.Len() < length {
		if builder.Len() > 0 {
			builder.WriteByte('/')
		}
		remaining := length - builder.Len()
		if remaining > maxPackagePathComponent {
			remaining = maxPackagePathComponent
		}
		builder.WriteString(strings.Repeat("z", remaining))
	}
	return builder.String()
}
