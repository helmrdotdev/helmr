package deployment

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInspectDependencySourceBun(t *testing.T) {
	root := t.TempDir()
	integrity := lockTestIntegrity()
	writeLockTestFile(t, root, "package.json", `{
		"name":"app",
		"packageManager":"bun@1.3.11",
		"workspaces":["packages/*"],
		"trustedDependencies":["native"]
	}`)
	writeLockTestFile(t, root, "packages/tool/package.json", `{
		"name":"tool",
		"version":"1.0.0"
	}`)
	writeLockTestFile(t, root, "bun.lock", `{
		// Bun owns the workspace pattern semantics.
		"lockfileVersion": 1,
		"configVersion": 1,
		"workspaces": {
			"": {
				"name": "app",
				"dependencies": {
					"alias": "npm:lodash@4.17.21",
					"tool": "workspace:*",
				},
			},
			"packages/tool": {
				"name": "tool",
				"version": "1.0.0",
			},
		},
		"packages": {
			"alias": ["lodash@4.17.21", "", {}, "`+integrity+`"],
			"tool": ["tool@workspace:packages/tool"],
		},
		"overrides": {"lodash": "4.17.21"},
		"catalog": {"zod": "4.4.3"},
		"catalogs": {"tools": {"typescript": "5.9.3"}},
	}`)

	source, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.Lockfile.Name != "bun.lock" ||
		source.Lockfile.Digest != digestBytes(source.LockfileBytes) {
		t.Fatalf("lockfile = %#v", source.Lockfile)
	}
	if got := lockTestManifestPaths(source); got != ".,packages/tool" {
		t.Fatalf("manifest paths = %q", got)
	}
	if len(source.RegistryPins) != 1 ||
		source.RegistryPins[0] != (RegistryPin{
			Name:      "lodash",
			Version:   "4.17.21",
			Integrity: integrity,
		}) {
		t.Fatalf("registry pins = %#v", source.RegistryPins)
	}
	if _, err := LocalManifestsDigest(source.LocalManifests); err != nil {
		t.Fatal(err)
	}
	writeLockTestFile(t, root, "packages/tool/src.ts", "export const value = 1;")
	writeLockTestFile(t, root, "dev/workflows/bun.lock", `{"lockfileVersion":0}`)
	again, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, again) {
		t.Fatal("non-manifest source changed dependency inputs")
	}
	writeLockTestFile(t, root, "dev/workflows/bun.lock", `{"lockfileVersion":1}`)
	afterNestedLockChange, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source, afterNestedLockChange) {
		t.Fatal("nested lockfile changed dependency inputs")
	}
}

func TestInspectDependencySourceBunV0Workspace(t *testing.T) {
	root := t.TempDir()
	writeLockTestFile(t, root, "package.json", `{
		"name":"app",
		"packageManager":"bun@1.3.11"
	}`)
	writeLockTestFile(t, root, "packages/tool/package.json", `{
		"name":"tool",
		"version":"1.0.0"
	}`)
	writeLockTestFile(t, root, "bun.lock", `{
		"lockfileVersion":0,
		"workspaces":{
			"":{"name":"app"},
			"packages/tool":{"name":"tool","version":"1.0.0"}
		},
		"packages":{
			"tool":["tool@workspace:packages/tool",{"dependencies":{}}]
		}
	}`)

	source, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := lockTestManifestPaths(source); got != ".,packages/tool" {
		t.Fatalf("manifest paths = %q", got)
	}
}

func TestInspectDependencySourceNPM(t *testing.T) {
	root := t.TempDir()
	integrity := lockTestIntegrity()
	writeLockTestFile(t, root, "package.json", `{
		"name":"app",
		"packageManager":"npm@11.4.2",
		"workspaces":["packages/*"],
		"dependencies":{"alias":"npm:lodash@4.17.21","tool":"*"},
		"overrides":{"lodash":"4.17.21"}
	}`)
	writeLockTestFile(t, root, "packages/tool/package.json", `{
		"name":"tool",
		"version":"1.0.0"
	}`)
	writeLockTestFile(t, root, "package-lock.json", `{
		"name":"app",
		"lockfileVersion":3,
		"requires":true,
		"packages":{
			"":{
				"name":"app",
				"workspaces":["packages/*"],
				"dependencies":{"alias":"npm:lodash@4.17.21","tool":"*"}
			},
			"packages/tool":{"name":"tool","version":"1.0.0"},
			"node_modules/tool":{"resolved":"packages/tool","link":true},
			"node_modules/alias":{
				"name":"lodash",
				"version":"4.17.21",
				"resolved":"https://registry.npmjs.org/lodash/-/lodash-4.17.21.tgz",
				"integrity":"`+integrity+`",
				"license":"MIT"
			}
		}
	}`)

	source, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.Lockfile.Name != "package-lock.json" {
		t.Fatalf("lockfile = %#v", source.Lockfile)
	}
	if got := lockTestManifestPaths(source); got != ".,packages/tool" {
		t.Fatalf("manifest paths = %q", got)
	}
	if len(source.RegistryPins) != 1 ||
		source.RegistryPins[0].Name != "lodash" {
		t.Fatalf("registry pins = %#v", source.RegistryPins)
	}
}

func TestDependencySourceRejectsUnsupportedGrammarAndInvalidSources(t *testing.T) {
	integrity := lockTestIntegrity()
	tests := []struct {
		name     string
		manager  PackageManager
		manifest string
		lockName string
		lock     string
		want     error
	}{
		{
			name:     "bun version",
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			manifest: `{"packageManager":"bun@1.3.11"}`,
			lockName: "bun.lock",
			lock:     `{"lockfileVersion":2,"workspaces":{"":{}},"packages":{}}`,
			want:     ErrLockfileUnsupported,
		},
		{
			name:     "bun unknown semantic field",
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			manifest: `{"packageManager":"bun@1.3.11"}`,
			lockName: "bun.lock",
			lock:     `{"lockfileVersion":1,"workspaces":{"":{}},"packages":{},"future":{}}`,
			want:     ErrLockfileUnsupported,
		},
		{
			name:     "bun git",
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			manifest: `{"packageManager":"bun@1.3.11"}`,
			lockName: "bun.lock",
			lock: `{"lockfileVersion":1,"workspaces":{"":{}},"packages":{
				"tool":["tool@github:owner/tool",{},"commit"]
			}}`,
			want: ErrDependencySourceInvalid,
		},
		{
			name:     "bun v0 workspace v1 tuple",
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			manifest: `{"packageManager":"bun@1.3.11"}`,
			lockName: "bun.lock",
			lock: `{"lockfileVersion":0,"workspaces":{
				"":{},
				"packages/tool":{"name":"tool"}
			},"packages":{
				"tool":["tool@workspace:packages/tool"]
			}}`,
			want: ErrLockfileUnsupported,
		},
		{
			name:     "bun v1 workspace v0 tuple",
			manager:  PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			manifest: `{"packageManager":"bun@1.3.11"}`,
			lockName: "bun.lock",
			lock: `{"lockfileVersion":1,"workspaces":{
				"":{},
				"packages/tool":{"name":"tool"}
			},"packages":{
				"tool":["tool@workspace:packages/tool",{}]
			}}`,
			want: ErrLockfileUnsupported,
		},
		{
			name:     "npm duplicate member",
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			manifest: `{"packageManager":"npm@11.4.2"}`,
			lockName: "package-lock.json",
			lock:     `{"lockfileVersion":3,"lockfileVersion":3,"packages":{"":{}}}`,
			want:     ErrLockfileUnsupported,
		},
		{
			name:     "npm arbitrary URL",
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			manifest: `{"packageManager":"npm@11.4.2"}`,
			lockName: "package-lock.json",
			lock: `{"lockfileVersion":3,"packages":{
				"":{},
				"node_modules/tool":{
					"version":"1.0.0",
					"resolved":"https://example.com/tool.tgz",
					"integrity":"` + integrity + `"
				}
			}}`,
			want: ErrDependencySourceInvalid,
		},
		{
			name:     "manifest file source",
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			manifest: `{"packageManager":"npm@11.4.2","dependencies":{"tool":"file:../tool"}}`,
			lockName: "package-lock.json",
			lock:     `{"lockfileVersion":3,"packages":{"":{}}}`,
			want:     ErrDependencySourceInvalid,
		},
		{
			name:     "automatic lifecycle",
			manager:  PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			manifest: `{"packageManager":"npm@11.4.2","scripts":{"dependencies":"node hook.js"}}`,
			lockName: "package-lock.json",
			lock:     `{"lockfileVersion":3,"packages":{"":{}}}`,
			want:     ErrDependencySourceInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(t, root, "package.json", test.manifest)
			writeLockTestFile(t, root, test.lockName, test.lock)
			_, err := InspectDependencySource(root, test.manager)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDependencySourceRejectsNestedLocalPackages(t *testing.T) {
	root := t.TempDir()
	writeLockTestFile(t, root, "package.json", `{"packageManager":"bun@1.3.11"}`)
	writeLockTestFile(t, root, "packages/a/package.json", `{"name":"a"}`)
	writeLockTestFile(t, root, "packages/a/nested/package.json", `{"name":"nested"}`)
	writeLockTestFile(t, root, "bun.lock", `{
		"lockfileVersion":1,
		"workspaces":{
			"":{},
			"packages/a":{"name":"a"},
			"packages/a/nested":{"name":"nested"}
		},
		"packages":{
			"a":["a@workspace:packages/a"],
			"nested":["nested@workspace:packages/a/nested"]
		}
	}`)
	_, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if !errors.Is(err, ErrDependencySourceInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestDependencySourceRejectsInvalidNPMRecordPaths(t *testing.T) {
	integrity := lockTestIntegrity()
	for _, packagePath := range []string{
		"/tmp/node_modules/tool",
		"node_modules/a/../node_modules/tool",
		"node_modules/@scope",
		"node_modules/a/lib",
	} {
		t.Run(packagePath, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(t, root, "package.json", `{"packageManager":"npm@11.4.2"}`)
			writeLockTestFile(t, root, "package-lock.json", fmt.Sprintf(`{
				"lockfileVersion":3,
				"packages":{
					"":{},
					%q:{
						"version":"1.0.0",
						"resolved":"https://registry.npmjs.org/tool/-/tool-1.0.0.tgz",
						"integrity":%q
					}
				}
			}`, packagePath, integrity))
			_, err := InspectDependencySource(
				root,
				PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			)
			if !errors.Is(err, ErrDependencySourceInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDependencySourceRejectsInvalidBunPackageKeys(t *testing.T) {
	integrity := lockTestIntegrity()
	for _, packagePath := range []string{
		"../tool",
		"/tool",
		"a/../tool",
		"@scope",
		"a//tool",
	} {
		t.Run(packagePath, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(t, root, "package.json", `{"packageManager":"bun@1.3.11"}`)
			writeLockTestFile(t, root, "bun.lock", fmt.Sprintf(`{
				"lockfileVersion":1,
				"workspaces":{"":{}},
				"packages":{%q:["tool@1.0.0","",{},%q]}
			}`, packagePath, integrity))
			_, err := InspectDependencySource(
				root,
				PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			)
			if !errors.Is(err, ErrDependencySourceInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDependencySourceRejectsNoncanonicalNPMLinks(t *testing.T) {
	for _, entry := range []string{
		`{"resolved":"packages/tool","link":false}`,
		`{"resolved":"packages/tool","link":true,"name":"tool"}`,
		`{"resolved":"packages/tool","link":true,"dependencies":{}}`,
	} {
		t.Run(entry, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(t, root, "package.json", `{"packageManager":"npm@11.4.2"}`)
			writeLockTestFile(t, root, "packages/tool/package.json", `{"name":"tool"}`)
			writeLockTestFile(t, root, "package-lock.json", fmt.Sprintf(`{
				"lockfileVersion":3,
				"packages":{
					"":{},
					"packages/tool":{"name":"tool"},
					"node_modules/tool":%s
				}
			}`, entry))
			_, err := InspectDependencySource(
				root,
				PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			)
			if !errors.Is(err, ErrLockfileUnsupported) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDependencySourceRegistryVersions(t *testing.T) {
	integrity := lockTestIntegrity()
	tests := []struct {
		version string
		valid   bool
	}{
		{version: "1.2.3", valid: true},
		{version: "1.2.3-beta.1", valid: true},
		{version: "latest"},
		{version: "^1.2.3"},
		{version: "v1.2.3"},
		{version: "1.2.3+build"},
	}
	for _, test := range tests {
		t.Run(test.version, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(t, root, "package.json", `{"packageManager":"bun@1.3.11"}`)
			writeLockTestFile(t, root, "bun.lock", fmt.Sprintf(`{
				"lockfileVersion":1,
				"workspaces":{"":{}},
				"packages":{"tool":["tool@%s","",{},%q]}
			}`, test.version, integrity))
			_, err := InspectDependencySource(
				root,
				PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
			)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && !errors.Is(err, ErrDependencySourceInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestDependencySourceRejectsProtocolRegistryVersion(t *testing.T) {
	root := t.TempDir()
	writeLockTestFile(t, root, "package.json", `{"packageManager":"npm@11.4.2"}`)
	writeLockTestFile(t, root, "package-lock.json", fmt.Sprintf(`{
		"lockfileVersion":3,
		"packages":{
			"":{},
			"node_modules/tool":{
				"version":"npm:other@1.2.3",
				"resolved":"https://registry.npmjs.org/tool/-/tool-1.2.3.tgz",
				"integrity":%q
			}
		}
	}`, lockTestIntegrity()))
	_, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
	)
	if !errors.Is(err, ErrDependencySourceInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestDependencySourceRejectsProhibitedSpecifierForms(t *testing.T) {
	for _, specifier := range []string{
		"git@github.com:owner/tool.git",
		"gitlab:owner/tool",
		"bitbucket:owner/tool",
		"owner/tool",
		"tool.tgz",
		`C:\tool`,
	} {
		t.Run(specifier, func(t *testing.T) {
			root := t.TempDir()
			writeLockTestFile(
				t,
				root,
				"package.json",
				fmt.Sprintf(
					`{"packageManager":"npm@11.4.2","dependencies":{"tool":%q}}`,
					specifier,
				),
			)
			writeLockTestFile(
				t,
				root,
				"package-lock.json",
				`{"lockfileVersion":3,"packages":{"":{}}}`,
			)
			_, err := InspectDependencySource(
				root,
				PackageManager{Name: PackageManagerNPM, Version: "11.4.2"},
			)
			if !errors.Is(err, ErrDependencySourceInvalid) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestUniqueJSONNestingBound(t *testing.T) {
	atLimit := lockTestNestedJSON(maxJSONNestingDepth)
	overLimit := lockTestNestedJSON(maxJSONNestingDepth + 1)
	if _, err := decodePackageManifest(atLimit); err != nil {
		t.Fatalf("package manifest at limit: %v", err)
	}
	if _, err := decodePackageManifest(overLimit); err == nil {
		t.Fatal("package manifest over limit was accepted")
	}
	if _, err := decodeLockJSON(atLimit, true); err != nil {
		t.Fatalf("lockfile at limit: %v", err)
	}
	if _, err := decodeLockJSON(overLimit, true); !errors.Is(err, ErrLockfileUnsupported) {
		t.Fatalf("lockfile error = %v", err)
	}
}

func TestDependencySourceRejectsConfigurationAlternateLockAndSymlink(t *testing.T) {
	manager := PackageManager{Name: PackageManagerNPM, Version: "11.4.2"}
	tests := []struct {
		mutate func(*testing.T, string)
		want   error
	}{
		{
			mutate: func(t *testing.T, root string) {
				writeLockTestFile(t, root, "tools/.npmrc", "registry=https://registry.npmjs.org")
			},
			want: ErrDependencySourceInvalid,
		},
		{
			mutate: func(t *testing.T, root string) {
				writeLockTestFile(t, root, "bun.lock", `{"lockfileVersion":1}`)
			},
			want: ErrDependencySourceInvalid,
		},
		{
			mutate: func(t *testing.T, root string) {
				writeLockTestFile(t, root, "npm-shrinkwrap.json", `{}`)
			},
			want: ErrDependencySourceInvalid,
		},
		{
			mutate: func(t *testing.T, root string) {
				writeLockTestFile(t, root, "real.json", `{"packageManager":"npm@11.4.2"}`)
				if err := os.Remove(filepath.Join(root, "package.json")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("real.json", filepath.Join(root, "package.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: ErrDependencySourceInvalid,
		},
	}
	for _, test := range tests {
		root := t.TempDir()
		writeLockTestFile(t, root, "package.json", `{"packageManager":"npm@11.4.2"}`)
		writeLockTestFile(t, root, "package-lock.json", `{
			"lockfileVersion":3,
			"packages":{"":{}}
		}`)
		test.mutate(t, root)
		if _, err := InspectDependencySource(root, manager); !errors.Is(
			err,
			test.want,
		) {
			t.Fatalf("error = %v", err)
		}
	}
}

func TestValidateDependencyGraphRequiresPreflightBounds(t *testing.T) {
	root := t.TempDir()
	integrity := lockTestIntegrity()
	writeLockTestFile(t, root, "package.json", `{
		"name":"app",
		"version":"1.0.0",
		"packageManager":"bun@1.3.11"
	}`)
	writeLockTestFile(t, root, "bun.lock", `{
		"lockfileVersion":1,
		"workspaces":{"":{"name":"app"}},
		"packages":{"zod":["zod@4.4.3","",{},"`+integrity+`"]}
	}`)
	source, err := InspectDependencySource(
		root,
		PackageManager{Name: PackageManagerBun, Version: "1.3.11"},
	)
	if err != nil {
		t.Fatal(err)
	}
	name := "app"
	version := "1.0.0"
	graph := PackageGraph{
		FormatVersion: PackageGraphFormatVersion,
		LocalPackages: []LocalPackage{{
			ManifestDigest: source.ManifestFiles[0].ManifestDigest,
			Name:           &name,
			Path:           ".",
			Version:        &version,
		}},
		RegistryPackages: []RegistryPackage{{
			InstallPath: "zod",
			Integrity:   integrity,
			Name:        "zod",
			Version:     "4.4.3",
		}},
		Resolutions: []PackageResolution{},
	}
	if err := ValidateDependencyGraph(source, graph); err != nil {
		t.Fatal(err)
	}
	otherName := "other"
	graph.LocalPackages[0].Name = &otherName
	if err := ValidateDependencyGraph(source, graph); !errors.Is(
		err,
		ErrDependencyOutputInvalid,
	) {
		t.Fatalf("name error = %v", err)
	}
	graph.LocalPackages[0].Name = &name
	otherVersion := "2.0.0"
	graph.LocalPackages[0].Version = &otherVersion
	if err := ValidateDependencyGraph(source, graph); !errors.Is(
		err,
		ErrDependencyOutputInvalid,
	) {
		t.Fatalf("version error = %v", err)
	}
	graph.LocalPackages[0].Version = &version
	graph.RegistryPackages[0].Version = "4.4.4"
	err = ValidateDependencyGraph(source, graph)
	if !errors.Is(err, ErrDependencyOutputInvalid) {
		t.Fatalf("error = %v", err)
	}
	if reason, ok := DependencyFailureReason(err); !ok ||
		reason != BuildFailureOutputInvalid {
		t.Fatalf("failure reason = %q, %t", reason, ok)
	}
}

func lockTestIntegrity() string {
	return "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64))
}

func lockTestNestedJSON(containers int) []byte {
	var builder strings.Builder
	builder.WriteString(`{"value":`)
	for range containers - 1 {
		builder.WriteByte('[')
	}
	builder.WriteString("null")
	for range containers - 1 {
		builder.WriteByte(']')
	}
	builder.WriteByte('}')
	return []byte(builder.String())
}

func writeLockTestFile(t *testing.T, root, name, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}

func lockTestManifestPaths(source DependencySource) string {
	paths := make([]string, 0, len(source.ManifestFiles))
	for _, manifest := range source.ManifestFiles {
		paths = append(paths, manifest.PackagePath)
	}
	return strings.Join(paths, ",")
}
