package definition

import (
	"encoding/json"
	"errors"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"testing"
)

func TestTemplatePoliciesResolveAndValidatePhaseOrigins(t *testing.T) {
	allowed := json.RawMessage(`{"allowDomains":["*.example.com"]}`)
	refs := []SecretBinding{{Secret: "TOKEN", Env: &SecretBindingEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}}}
	runtime, stage, err := ResolveTemplatePolicies(allowed, nil, true, refs, refs)
	if err != nil || runtime.Kind != networkpolicy.Allowlist || stage.Kind != networkpolicy.Allowlist {
		t.Fatalf("inheritance %+v %+v %v", runtime, stage, err)
	}
	_, stage, err = ResolveTemplatePolicies(nil, nil, false, []SecretBinding{}, []SecretBinding{})
	if err != nil || stage.Kind != networkpolicy.Blocked {
		t.Fatal("seed policy", stage, err)
	}
	for _, tc := range []struct {
		network, prepare json.RawMessage
		callback         bool
		runtime, prep    []SecretBinding
	}{
		{allowed, json.RawMessage(`{"blockAll":true}`), false, nil, nil},
		{nil, nil, false, nil, refs},
		{json.RawMessage(`{"blockAll":true}`), nil, true, refs, nil},
		{allowed, json.RawMessage(`{"blockAll":true}`), true, nil, refs},
		{json.RawMessage(`{"allowDomains":["other.example.com"]}`), nil, true, refs, nil},
	} {
		if _, _, err := ResolveTemplatePolicies(tc.network, tc.prepare, tc.callback, tc.runtime, tc.prep); err == nil {
			t.Fatalf("accepted invalid phase %+v", tc)
		}
	}
}
func TestBuildPlanResolvesMcpKeysAcrossCompleteManifest(t *testing.T) {
	plan, _ := fixtureBuildPlan(t)
	plan.Definitions[0].Agent.RuntimeMcp = json.RawMessage(`{"allow":[{"operation":"spawn","agents":["missing"]}]}`)
	if err := ValidateBuildPlan(plan); !errors.Is(err, ErrInvalidRuntimeMcpPolicy) {
		t.Fatalf("unknown Agent accepted or identity lost: %v", err)
	}
	plan.Definitions[0].Agent.RuntimeMcp = json.RawMessage(`{"allow":[{"operation":"spawn","agents":["coder"]}]}`)
	if err := ValidateBuildPlan(plan); err != nil {
		t.Fatal(err)
	}
}
