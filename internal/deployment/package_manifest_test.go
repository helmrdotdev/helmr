package deployment

import (
	"strings"
	"testing"
)

func TestParsePackageManifest(t *testing.T) {
	manifest, err := parsePackageManifest([]byte(`{
		"name":"@scope/tool",
		"version":"1.2.3",
		"type":"module",
		"bin":{"Build":"./bin/build.mjs","check":"bin/check"}
	}`))
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
	if manifest.Bins["Build"] != "bin/build.mjs" || manifest.Bins["check"] != "bin/check" {
		t.Fatalf("bins = %#v", manifest.Bins)
	}
}

func TestParsePackageManifestDerivesStringBinCommand(t *testing.T) {
	manifest, err := parsePackageManifest([]byte(`{"name":"@scope/tool","bin":"./cli.js"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Bins) != 1 || manifest.Bins["tool"] != "cli.js" {
		t.Fatalf("bins = %#v", manifest.Bins)
	}
}

func TestParsePackageManifestRecordsAutomaticScripts(t *testing.T) {
	manifest, err := parsePackageManifest([]byte(`{
		"scripts":{
			"test":"bun test",
			"prepare":null,
			"preinstall":"node setup.js"
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(manifest.AutomaticScripts, ",") != "preinstall,prepare" {
		t.Fatalf("automatic scripts = %#v", manifest.AutomaticScripts)
	}
}

func TestParsePackageManifestRejectsDuplicateMembersAtEveryDepth(t *testing.T) {
	for _, raw := range []string{
		`{"name":"tool","name":"other"}`,
		`{"name":"tool","config":{"mode":1,"mode":2}}`,
		`{"name":"tool","config":[{"mode":1,"mode":2}]}`,
	} {
		if _, err := parsePackageManifest([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), "duplicate object member") {
			t.Fatalf("parsePackageManifest(%q) error = %v", raw, err)
		}
	}
}

func TestParsePackageManifestRejectsUnsupportedShape(t *testing.T) {
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
			if _, err := parsePackageManifest([]byte(test.raw)); err == nil {
				t.Fatal("parsePackageManifest returned nil error")
			}
		})
	}
}

func TestParsePackageManifestRejectsOversize(t *testing.T) {
	raw := make([]byte, maxPackageManifestSizeBytes+1)
	raw[0] = '{'
	raw[len(raw)-1] = '}'
	if _, err := parsePackageManifest(raw); err == nil {
		t.Fatal("parsePackageManifest returned nil error")
	}
}
