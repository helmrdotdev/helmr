package secretname

import "testing"

func TestValidate(t *testing.T) {
	for _, name := range []string{"API_TOKEN", "config-json", "a.b", "0abc"} {
		if err := Validate(name); err != nil {
			t.Fatalf("Validate(%q) = %v", name, err)
		}
	}
	for _, name := range []string{"", "-bad", "bad/name", "bad name"} {
		if err := Validate(name); err == nil {
			t.Fatalf("Validate(%q) succeeded", name)
		}
	}
}
