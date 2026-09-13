package workspace

import "testing"

func TestSecretAuthorityCeiling(t *testing.T) {
	source := []SecretAuthority{{ID: "a", Mode: "protected", Origins: []string{"https://api.example.com"}}}
	for _, target := range [][]SecretAuthority{
		{{ID: "a", Mode: "raw"}},
		{{ID: "b", Mode: "protected", Origins: []string{"https://api.example.com"}}},
		{{ID: "a", Mode: "protected", Origins: []string{"https://api.example.com", "https://other.example.com"}}},
	} {
		if AllowsSecretAuthority(source, target) {
			t.Fatalf("escalation accepted: %+v", target)
		}
	}
	if !AllowsSecretAuthority(source, source) || !AllowsSecretAuthority(source, nil) {
		t.Fatal("narrowing rejected")
	}
	source = append(source, SecretAuthority{ID: "a", Mode: "raw"})
	if !AllowsSecretAuthority(source, []SecretAuthority{{ID: "a", Mode: "raw"}, {ID: "a", Mode: "protected", Origins: []string{"https://other.example.com"}}}) {
		t.Fatal("raw superset rejected")
	}
}
