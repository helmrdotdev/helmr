package mcppolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeAndSubset(t *testing.T) {
	max, err := Normalize(json.RawMessage(`{"allow":[{"operation":"spawn","agents":["b","a"]},{"operation":"spawn","agents":["c"]},{"operation":"list_computers"}]}`), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"kind":"allow","rules":[{"operation":"list_computers"},{"operation":"spawn","agents":["a","b","c"],"computer":"new"}]}`
	if string(max.Bytes()) != want {
		t.Fatalf("%s", max.Bytes())
	}
	subset, err := Normalize(json.RawMessage(`{"allow":[{"operation":"spawn","agents":["b"],"computer":"new"}]}`), []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if !subset.SubsetOf(max) || max.SubsetOf(subset) || subset.Allows("spawn", "b", "caller") || subset.Allows("inspect_turn", "b", "") {
		t.Fatal("grant widened")
	}
	if decoded, err := Decode(max.Bytes()); err != nil || !decoded.Equal(max) || decoded.Digest() != max.Digest() {
		t.Fatalf("roundtrip %v", err)
	}
	empty, err := Normalize(json.RawMessage(`{"allow":[]}`), []string{})
	if err != nil || string(empty.Bytes()) != `{"kind":"allow","rules":[]}` || !empty.SubsetOf(subset) || empty.Has("spawn") {
		t.Fatal("empty grants lost", err)
	}
	unrestricted, err := Normalize(nil, nil)
	if err != nil || !max.SubsetOf(unrestricted) || unrestricted.SubsetOf(max) || unrestricted.Public() != nil {
		t.Fatal("unrestricted semantics")
	}
}
func TestRejectInvalidPolicies(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `true`, `{"blockAll":true}`, `{"allow":null}`, `{"allow":[],"extra":0}`, `{"allow":[],"allow":[]}`, `{"allow":[{"operation":"spawn","agents":["a"],"computer":null}]}`, `{"allow":[{"operation":"spawn","agents":["a"],"computer":""}]}`, `{"allow":[{"operation":"inspect_turn","agents":["a"],"computer":""}]}`, `{"allow":[{"operation":"list_computers","agents":null}]}`, `{"allow":[{"operation":"list_computers","computer":null}]}`, `{"allow":[{"operation":"wait_turn","agents":["a"]}]}`, `{"allow":[{"operation":"spawn","agents":["unknown"]}]}`, `{"allow":[{"operation":"spawn","agents":["a","a"]}]}`, `{"allow":[{"operation":"spawn","agents":["*"]}]}`} {
		t.Run(raw, func(t *testing.T) {
			if _, err := Normalize(json.RawMessage(raw), []string{"a"}); err == nil {
				t.Fatal("accepted invalid")
			}
		})
	}
}
func TestRejectCorruptStorage(t *testing.T) {
	for _, raw := range []string{``, `null`, `{}`, `{"kind":"unrestricted","rules":null}`, `{"kind":"unrestricted","other":1}`, `{"kind":"allow"}`, `{"kind":"allow","rules":null}`, `{"kind":"allow","rules":[{"operation":"spawn","agents":["a"]}]}`, `{"kind":"allow","rules":[{"operation":"spawn","agents":["bad key"],"computer":"new"}]}`} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestDerivedOperationsExplainCanonicalGrantsWithoutTargets(t *testing.T) {
	for operation, hint := range map[string]string{"wait_turn": "author inspect_turn instead", "list_sessions": "author inspect_session instead", "list_agents": "author start or spawn instead", "skills": "local guidance derived from descriptor capabilities"} {
		for _, agents := range []string{"", `,"agents":["a"]`} {
			raw := json.RawMessage(`{"allow":[{"operation":"` + operation + `"` + agents + `}]}`)
			if _, err := Normalize(raw, []string{"a"}); err == nil || !strings.Contains(err.Error(), hint) {
				t.Fatalf("%s lost its operation guidance: %v", raw, err)
			}
		}
	}
	for _, operation := range []string{"unknown", ""} {
		if _, err := Normalize(json.RawMessage(`{"allow":[{"operation":"`+operation+`"}]}`), []string{"a"}); err == nil {
			t.Fatalf("unsupported %q became a grant", operation)
		}
	}
}
