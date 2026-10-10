package definition

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

func fixtureBuildPlan(t *testing.T) (BuildPlan, []byte) {
	t.Helper()
	raw, err := os.ReadFile("testdata/agent-build-plan.json")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := ParseBuildPlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	return plan, raw
}
func TestAgentBuildPlanCompilerContract(t *testing.T) {
	plan, raw := fixtureBuildPlan(t)
	if len(plan.Definitions) != 2 || plan.Definitions[0].Agent.ComputerDefinitionID != "workspace" || *plan.Definitions[0].Agent.MaxTurnDurationMs != 300000 || plan.Definitions[1].Computer.BuildSecrets[0].SecretID != "01900000-0000-7000-8000-000000000002" {
		t.Fatalf("unexpected compiler projection: %+v", plan)
	}
	got, err := CanonicalBuildPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatalf("roundtrip changed compiler bytes: %s", got)
	}
}
func TestAgentBuildPlanRejectsMalformedAuthority(t *testing.T) {
	tests := map[string]func(*BuildPlan){
		"missing Computer":     func(p *BuildPlan) { p.Definitions[0].Agent.ComputerDefinitionID = "absent" },
		"duplicate definition": func(p *BuildPlan) { p.Definitions = append(p.Definitions, p.Definitions[1]) },
		"definition order":     func(p *BuildPlan) { p.Definitions[0], p.Definitions[1] = p.Definitions[1], p.Definitions[0] },
		"mixed manifest":       func(p *BuildPlan) { p.Definitions[0].Computer = p.Definitions[1].Computer },
		"negative duration":    func(p *BuildPlan) { p.Definitions[0].Agent.MaxTurnDurationMs = new(int64(-1)) },
		"unsafe duration":      func(p *BuildPlan) { p.Definitions[0].Agent.MaxTurnDurationMs = new(int64(9007199254740992)) },
		"malformed scheduled conversation": func(p *BuildPlan) {
			p.Definitions[0].Agent.Triggers["nightly"] = CronTrigger{Cron: "* * * * *", Timezone: "UTC", Input: []byte(`{"type":"message","content":"shorthand"}`)}
		},
		"missing triggers": func(p *BuildPlan) { p.Definitions[0].Agent.Triggers = nil },
		"invalid cron": func(p *BuildPlan) {
			p.Definitions[0].Agent.Triggers["nightly"] = CronTrigger{Cron: "bad", Timezone: "UTC", Input: json.RawMessage(`[]`)}
		},
		"invalid timezone": func(p *BuildPlan) {
			v := p.Definitions[0].Agent.Triggers["nightly"]
			v.Timezone = "Local"
			p.Definitions[0].Agent.Triggers["nightly"] = v
		},
		"missing timezone": func(p *BuildPlan) {
			v := p.Definitions[0].Agent.Triggers["nightly"]
			v.Timezone = ""
			p.Definitions[0].Agent.Triggers["nightly"] = v
		},
		"missing trigger input": func(p *BuildPlan) {
			v := p.Definitions[0].Agent.Triggers["nightly"]
			v.Input = nil
			p.Definitions[0].Agent.Triggers["nightly"] = v
		},
		"invalid disk":             func(p *BuildPlan) { p.Definitions[1].Computer.Resources.DiskMiB = new(int64(0)) },
		"invalid freshness":        func(p *BuildPlan) { p.Definitions[1].Computer.Refresh.MaxAgeMs = new(int64(0)) },
		"missing runtime bindings": func(p *BuildPlan) { p.Definitions[1].Computer.Secrets = nil },
		"missing build bindings":   func(p *BuildPlan) { p.Definitions[1].Computer.BuildSecrets = nil },
		"invalid build Secret": func(p *BuildPlan) {
			p.Definitions[1].Computer.BuildSecrets[0].SecretID = "name"
		},
		"invalid runtime Secret": func(p *BuildPlan) {
			p.Definitions[1].Computer.Secrets[0].SecretID = "00000000-0000-0000-0000-000000000000"
		},
		"invalid image": func(p *BuildPlan) { p.Definitions[1].Computer.ImageBuild.Images[0].Steps = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p, _ := fixtureBuildPlan(t)
			mutate(&p)
			if err := ValidateBuildPlan(p); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
}
func TestAgentBuildPlanRequiresClosedCanonicalShape(t *testing.T) {
	_, raw := fixtureBuildPlan(t)
	for name, change := range map[string]func([]byte) []byte{
		"whitespace": func(b []byte) []byte { return append([]byte(" "), b...) },
		"old kind":   func(b []byte) []byte { return bytes.Replace(b, []byte(`"kind":"agent"`), []byte(`"kind":"task"`), 1) },
		"unknown policy": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"setup":true`), []byte(`"setup":true,"unrecognized":1`), 1)
		},
		"unknown secret member": func(b []byte) []byte {
			return bytes.Replace(b, []byte(`"secretId":"01900000`), []byte(`"unrecognized":1,"secretId":"01900000`), 1)
		},
		"missing boolean": func(b []byte) []byte { return bytes.Replace(b, []byte(`"setup":true,`), nil, 1) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := change(raw)
			if bytes.Equal(candidate, raw) {
				t.Fatal("invalid-shape fixture did not change")
			}
			if name != "whitespace" {
				var err error
				candidate, err = jsoncanon.Transform(candidate)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := ParseBuildPlan(candidate); err == nil {
				t.Fatal("invalid shape accepted")
			}
		})
	}
}
