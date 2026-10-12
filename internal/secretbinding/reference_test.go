package secretbinding

import (
	"reflect"
	"testing"
)

func TestStableReferencesCanonicalizePlacementsWithoutMutatingDeclarations(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	input := []Reference{
		{Secret: id, File: &File{Path: "/etc/service/token"}},
		{Secret: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://API.EXAMPLE.COM:443", "https://api.example.com"}}},
		{Secret: id, Env: &ReferenceEnv{Name: "RAW_TOKEN", Mode: "raw"}},
	}
	before := CloneReferences(input)
	actual, err := CanonicalReferences(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := []Reference{
		{Secret: id, Env: &ReferenceEnv{Name: "RAW_TOKEN", Mode: "raw"}},
		{Secret: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}},
		{Secret: id, File: &File{Path: "/etc/service/token"}},
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("canonical placements: %+v", actual)
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("normalization mutated declaration")
	}
}

func TestStableReferencesRequireCanonicalIdentityAndOnePlacement(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	for name, input := range map[string][]Reference{
		"missing list":      nil,
		"missing placement": {{Secret: id}},
		"both placements":   {{Secret: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}, File: &File{Path: "/etc/token"}}},
		"space in name":     {{Secret: "bad name", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"invalid character": {{Secret: "/token", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"empty identity":    {{Secret: "", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"implicit mode":     {{Secret: id, Env: &ReferenceEnv{Name: "TOKEN"}}},
		"missing origins":   {{Secret: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalReferences(input); err == nil {
				t.Fatal("invalid binding accepted")
			}
		})
	}
	if result, err := CanonicalReferences([]Reference{}); err != nil || result == nil {
		t.Fatalf("explicit empty list: %v", err)
	}
}
