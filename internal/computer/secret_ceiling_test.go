package computer

import "testing"

func TestSecretAuthorityCeiling(t *testing.T) {
	source := []secretAuthority{{ID: "a", Mode: "protected", Origins: []string{"https://api.example.com"}}}
	for _, target := range [][]secretAuthority{
		{{ID: "a", Mode: "raw"}},
		{{ID: "b", Mode: "protected", Origins: []string{"https://api.example.com"}}},
		{{ID: "a", Mode: "protected", Origins: []string{"https://api.example.com", "https://other.example.com"}}},
	} {
		if allowsSecretAuthority(source, target) {
			t.Fatalf("escalation accepted: %+v", target)
		}
	}
	if !allowsSecretAuthority(source, source) || !allowsSecretAuthority(source, nil) {
		t.Fatal("narrowing rejected")
	}
	source = append(source, secretAuthority{ID: "a", Mode: "raw"})
	if !allowsSecretAuthority(source, []secretAuthority{{ID: "a", Mode: "raw"}, {ID: "a", Mode: "protected", Origins: []string{"https://other.example.com"}}}) {
		t.Fatal("raw superset rejected")
	}
}
