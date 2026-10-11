package secretbinding

import (
	"reflect"
	"testing"
)

func TestStableReferencesCanonicalizePlacementsWithoutMutatingDeclarations(t *testing.T) {
	const id = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	input := []Reference{
		{SecretID: id, File: &File{Path: "/etc/service/token"}},
		{SecretID: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://API.EXAMPLE.COM:443", "https://api.example.com"}}},
		{SecretID: id, Env: &ReferenceEnv{Name: "RAW_TOKEN", Mode: "raw"}},
	}
	before := CloneReferences(input)
	actual, err := CanonicalReferences(input)
	if err != nil {
		t.Fatal(err)
	}
	expected := []Reference{
		{SecretID: id, Env: &ReferenceEnv{Name: "RAW_TOKEN", Mode: "raw"}},
		{SecretID: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected", AllowedOrigins: []string{"https://api.example.com"}}},
		{SecretID: id, File: &File{Path: "/etc/service/token"}},
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
		"missing list":          nil,
		"missing placement":     {{SecretID: id}},
		"both placements":       {{SecretID: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}, File: &File{Path: "/etc/token"}}},
		"name":                  {{SecretID: "token", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"noncanonical identity": {{SecretID: "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"nil identity":          {{SecretID: "00000000-0000-0000-0000-000000000000", Env: &ReferenceEnv{Name: "TOKEN", Mode: "raw"}}},
		"implicit mode":         {{SecretID: id, Env: &ReferenceEnv{Name: "TOKEN"}}},
		"missing origins":       {{SecretID: id, Env: &ReferenceEnv{Name: "TOKEN", Mode: "protected"}}},
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
