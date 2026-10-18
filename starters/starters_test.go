package starters

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
)

func TestCommittedSettingsMatchTheDefaultRendering(t *testing.T) {
	for _, id := range IDs() {
		starter, _ := Lookup(id)
		binding, err := starter.Binding("")
		if err != nil {
			t.Fatal(err)
		}
		committed, err := fs.ReadFile(starter.Files(), SettingsFile)
		if err != nil {
			t.Fatal(err)
		}
		if want := Settings("assistant", binding); string(committed) != want {
			t.Fatalf("%s: %s is out of date; want:\n%s", id, SettingsFile, want)
		}
	}
}

func TestSettingsNameTheNamesakeOnlyForPoolNames(t *testing.T) {
	binding := Binding{Provider: providers["anthropic"], Model: "claude-sonnet-5-5", Env: "ANTHROPIC_API_KEY"}
	pooled := Settings("noether", binding)
	if !strings.Contains(pooled, `"You are noether, a Helmr agent named after the mathematician Emmy Noether.`) {
		t.Fatalf("pooled settings = %s", pooled)
	}
	custom := Settings("support", binding)
	if strings.Contains(custom, "named after") || !strings.Contains(custom, `export const agentName = "support"`) {
		t.Fatalf("custom settings = %s", custom)
	}
}

func TestNamesakesAreValidUniqueAgentNames(t *testing.T) {
	seen := map[string]bool{}
	for _, namesake := range namesakes {
		if !ValidAgentName(namesake.Name) || len(namesake.Name) > 10 || seen[namesake.Name] || namesake.Person == "" || namesake.Field == "" {
			t.Fatalf("bad namesake %+v", namesake)
		}
		seen[namesake.Name] = true
	}
	if !seen[RandomName()] {
		t.Fatal("random name outside the pool")
	}
}

func TestPackageJSONUsesTheReleasedSDK(t *testing.T) {
	starter, _ := Lookup("claude-code")
	pkg, err := starter.PackageJSON("1.2.3", "24.21.0")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Dependencies map[string]string
		Overrides    map[string]string
		DevEngines   struct{ Runtime struct{ Version string } }
	}
	if err := json.Unmarshal([]byte(pkg), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Dependencies["@helmr/sdk"] != "1.2.3" || parsed.Dependencies["@anthropic-ai/claude-agent-sdk"] == "" || parsed.Overrides != nil || parsed.DevEngines.Runtime.Version != "24.21.0" {
		t.Fatalf("package.json = %s", pkg)
	}
	if strings.Contains(pkg, "file:") {
		t.Fatalf("package.json references local files: %s", pkg)
	}
}

func TestProjectFilesRenameTheAgentAndRejectCollisions(t *testing.T) {
	starter, _ := Lookup("claude-code")
	paths, err := starter.ProjectFiles("noether")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "agents/noether.ts") || strings.Contains(joined, AgentFile) || strings.Contains(joined, "tests/") {
		t.Fatalf("paths = %v", paths)
	}
	if _, err := starter.ProjectFiles("settings"); err == nil {
		t.Fatal("name colliding with settings.ts was accepted")
	}
}

func TestBindingRejectsUnsupportedProviders(t *testing.T) {
	starter, _ := Lookup("claude-code")
	if _, err := starter.Binding("openai"); err == nil || !strings.Contains(err.Error(), "supported: anthropic") {
		t.Fatalf("err = %v", err)
	}
}
