package deployment

import (
	"strings"
	"testing"
)

func TestParseLocalPackageManifest(t *testing.T) {
	manifest, err := parseLocalPackageManifest([]byte(`{
		"name":"@scope/tool",
		"version":"1.2.3",
		"type":"module",
		"packageManager":"bun@1.3.10",
		"bin":{"Build":"./bin/build.mjs","check":"bin/check"}
	}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name == nil || *manifest.Name != "@scope/tool" {
		t.Fatalf("name = %v", manifest.Name)
	}
	if manifest.Version == nil || *manifest.Version != "1.2.3" {
		t.Fatalf("version = %v", manifest.Version)
	}
	if manifest.Type != "module" {
		t.Fatalf("type = %q", manifest.Type)
	}
	if manifest.PackageManager == nil || *manifest.PackageManager != "bun@1.3.10" {
		t.Fatalf("package manager = %v", manifest.PackageManager)
	}
	if manifest.Bins["Build"] != "bin/build.mjs" || manifest.Bins["check"] != "bin/check" {
		t.Fatalf("bins = %#v", manifest.Bins)
	}
}

func TestParseLocalPackageManifestDerivesStringBinCommand(t *testing.T) {
	manifest, err := parseLocalPackageManifest(
		[]byte(`{"name":"@scope/tool","bin":"./cli.js"}`),
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Bins) != 1 || manifest.Bins["tool"] != "cli.js" {
		t.Fatalf("bins = %#v", manifest.Bins)
	}
}

func TestParseLocalPackageManifestRecordsAutomaticScripts(t *testing.T) {
	manifest, err := parseLocalPackageManifest([]byte(`{
		"scripts":{
			"test":"bun test",
			"prepare":null,
			"preinstall":"node setup.js"
		}
	}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.AutomaticScripts, ",") != "preinstall,prepare" {
		t.Fatalf("automatic scripts = %#v", manifest.AutomaticScripts)
	}
}

func TestParsePackageScopeRejectsDuplicateMembersAtEveryDepth(t *testing.T) {
	for _, raw := range []string{
		`{"name":"tool","name":"other"}`,
		`{"name":"tool","config":{"mode":1,"mode":2}}`,
		`{"name":"tool","config":[{"mode":1,"mode":2}]}`,
	} {
		if _, err := parsePackageScope([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), "duplicate object member") {
			t.Fatalf("parsePackageScope(%q) error = %v", raw, err)
		}
	}
}

func TestParseLocalPackageManifestRejectsUnsupportedShape(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "root", raw: `[]`},
		{name: "null name", raw: `{"name":null}`},
		{name: "invalid type", raw: `{"type":"dual"}`},
		{name: "string bin without name", raw: `{"bin":"cli.js"}`},
		{
			name: "invalid derived command",
			raw:  `{"name":"` + strings.Repeat("a", 129) + `","bin":"cli.js"}`,
		},
		{name: "null bin", raw: `{"name":"tool","bin":null}`},
		{name: "invalid command", raw: `{"name":"tool","bin":{"../tool":"cli.js"}}`},
		{name: "absolute target", raw: `{"name":"tool","bin":"/cli.js"}`},
		{name: "double dot target", raw: `{"name":"tool","bin":"../cli.js"}`},
		{name: "second dot prefix", raw: `{"name":"tool","bin":"././cli.js"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseLocalPackageManifest([]byte(test.raw), true); err == nil {
				t.Fatal("parseLocalPackageManifest returned nil error")
			}
		})
	}
}

func TestParsePackageScopeRejectsOversize(t *testing.T) {
	raw := make([]byte, maxPackageManifestSizeBytes+1)
	raw[0] = '{'
	raw[len(raw)-1] = '}'
	if _, err := parsePackageScope(raw); err == nil {
		t.Fatal("parsePackageScope returned nil error")
	}
}

func TestParsePackageScopeIgnoresGraphRootFields(t *testing.T) {
	manifest, err := parsePackageScope([]byte(`{
		"bin":null,
		"name":null,
		"packageManager":null,
		"scripts":{"prepare":"node generate.js"},
		"type":"module",
		"version":null
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Type != "module" || len(manifest.Bins) != 0 ||
		manifest.Name != nil || manifest.Version != nil || manifest.PackageManager != nil ||
		len(manifest.AutomaticScripts) != 0 {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestParsePackageScopeRejectsInvalidType(t *testing.T) {
	if _, err := parsePackageScope([]byte(`{"type":"dual"}`)); err == nil {
		t.Fatal("parsePackageScope returned nil error")
	}
}
