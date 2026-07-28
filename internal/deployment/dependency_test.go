package deployment

import (
	"strings"
	"testing"
)

func TestValidateSourceDependenciesAcceptsBoundStandardOrigins(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tests := []struct {
		name     string
		manager  PackageManagerName
		manifest string
		lockfile string
	}{
		{
			name:    "npm archive",
			manager: PackageManagerNPM,
			manifest: `{
				"dependencies":{"fixture":"https://packages.example.com/fixture.tgz"}
			}`,
			lockfile: `{
				"lockfileVersion":3,
				"packages":{
					"node_modules/fixture":{
						"integrity":"sha512-AAAAAAAA",
						"resolved":"https://packages.example.com/fixture.tgz"
					}
				}
			}`,
		},
		{
			name:    "npm git",
			manager: PackageManagerNPM,
			manifest: `{
				"dependencies":{"fixture":"git+https://github.com/example/fixture.git#` + commit + `"}
			}`,
			lockfile: `{
				"lockfileVersion":3,
				"packages":{
					"node_modules/fixture":{
						"resolved":"git+https://github.com/example/fixture.git#` + commit + `"
					}
				}
			}`,
		},
		{
			name:    "pnpm archive",
			manager: PackageManagerPNPM,
			manifest: `{
				"dependencies":{"fixture":"https://packages.example.com/fixture.tgz"}
			}`,
			lockfile: `lockfileVersion: '9.0'
importers:
  .:
    dependencies:
      fixture:
        specifier: https://packages.example.com/fixture.tgz
        version: https://packages.example.com/fixture.tgz
packages:
  fixture@https://packages.example.com/fixture.tgz:
    resolution:
      integrity: sha512-AAAAAAAA
      tarball: https://packages.example.com/fixture.tgz
`,
		},
		{
			name:    "Bun archive",
			manager: PackageManagerBun,
			manifest: `{
				"dependencies":{"fixture":"https://packages.example.com/fixture.tgz"}
			}`,
			lockfile: `{
				"lockfileVersion": 1,
				"workspaces": {},
				"packages": {
					"fixture": [
						"https://packages.example.com/fixture.tgz",
						"",
						{},
						"sha512-AAAAAAAA",
					],
				},
			}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, err := decodePackageManifest([]byte(test.manifest))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateSourceDependencies(
				manifest,
				test.manager,
				selectedSourceLockfile{name: "lockfile", raw: []byte(test.lockfile)},
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateSourceDependenciesRejectsUnboundAuthority(t *testing.T) {
	commit := strings.Repeat("a", 40)
	tests := []struct {
		name     string
		manifest string
		lockfile string
	}{
		{
			name:     "plain HTTP",
			manifest: `{}`,
			lockfile: `{"packages":{"node_modules/fixture":{"integrity":"sha512-AAAAAAAA","resolved":"http://packages.example.com/fixture.tgz"}}}`,
		},
		{
			name:     "URL userinfo",
			manifest: `{}`,
			lockfile: `{"packages":{"node_modules/fixture":{"integrity":"sha512-AAAAAAAA","resolved":"https://user:password@packages.example.com/fixture.tgz"}}}`,
		},
		{
			name:     "private destination",
			manifest: `{}`,
			lockfile: `{"packages":{"node_modules/fixture":{"integrity":"sha512-AAAAAAAA","resolved":"https://127.0.0.1/fixture.tgz"}}}`,
		},
		{
			name:     "mutable git ref",
			manifest: `{"dependencies":{"fixture":"git+https://github.com/example/fixture.git#main"}}`,
			lockfile: `{"packages":{}}`,
		},
		{
			name:     "credentialed git transport",
			manifest: `{"dependencies":{"fixture":"git+ssh://git@github.com/example/fixture.git#` + commit + `"}}`,
			lockfile: `{"packages":{}}`,
		},
		{
			name:     "archive without integrity",
			manifest: `{"dependencies":{"fixture":"https://packages.example.com/fixture.tgz"}}`,
			lockfile: `{"packages":{"node_modules/fixture":{"resolved":"https://packages.example.com/fixture.tgz"}}}`,
		},
		{
			name:     "archive missing from lockfile",
			manifest: `{"dependencies":{"fixture":"https://packages.example.com/fixture.tgz"}}`,
			lockfile: `{"packages":{}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest, err := decodePackageManifest([]byte(test.manifest))
			if err != nil {
				t.Fatal(err)
			}
			if err := validateSourceDependencies(
				manifest,
				PackageManagerNPM,
				selectedSourceLockfile{
					name: "package-lock.json",
					raw:  []byte(test.lockfile),
				},
			); err == nil {
				t.Fatal("validateSourceDependencies returned nil error")
			}
		})
	}
}

func TestNormalizeJSONCPreservesStringsAndRemovesSyntaxExtensions(t *testing.T) {
	raw := []byte(`{
		"url": "https://packages.example.com/path//part",
		"values": [1, 2,],
		// line comment
		"nested": {"value": true,}, /* block
		comment */
	}`)
	normalized, err := normalizeJSONC(raw)
	if err != nil {
		t.Fatal(err)
	}
	value, err := decodeDependencyJSON(normalized, "JSONC fixture")
	if err != nil {
		t.Fatal(err)
	}
	object, ok := value.(map[string]any)
	if !ok || object["url"] != "https://packages.example.com/path//part" {
		t.Fatalf("normalized JSONC = %#v", value)
	}
}
