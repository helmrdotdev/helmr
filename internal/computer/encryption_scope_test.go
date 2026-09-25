package computer

import (
	"strings"
	"testing"
)

func TestEncryptionScope(t *testing.T) {
	seen := map[string]bool{}
	for _, ids := range [][3]string{
		{"org", "env", "computer"}, {"other", "env", "computer"},
		{"org", "other", "computer"}, {"org", "env", "other"},
		{"org/env", "a", "b"}, {"org", "env/a", "b"},
		{`org","env`, "a", "b"},
	} {
		scope, err := EncryptionScope(ids[0], ids[1], ids[2])
		if err != nil {
			t.Fatal(err)
		}
		if seen[scope] {
			t.Fatalf("identity collision: %q", ids)
		}
		seen[scope] = true
		again, err := EncryptionScope(ids[0], ids[1], ids[2])
		if err != nil || again != scope {
			t.Fatal("scope is not canonical")
		}
	}
	for _, ids := range [][3]string{{"", "e", "c"}, {"o", "", "c"}, {"o", "e", ""}, {"o", "e", string([]byte{255})}, {"o", "e", strings.Repeat("x", 256)}} {
		if _, err := EncryptionScope(ids[0], ids[1], ids[2]); err == nil {
			t.Fatalf("invalid identity accepted: %q", ids)
		}
	}
	// Production UUID identities fit without changing the qualified codec framing.
	id := "01997f91-0564-7000-a000-000000000001"
	if _, err := EncryptionScope(id, id, id); err != nil {
		t.Fatal(err)
	}
}
