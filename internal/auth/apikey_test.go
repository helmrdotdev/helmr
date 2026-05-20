package auth

import (
	"strings"
	"testing"
)

func TestGenerateAPIKeyUsesHelmrPrefix(t *testing.T) {
	key, err := GenerateAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key.Raw, APIKeyPrefix) {
		t.Fatalf("raw key = %q, want prefix %q", key.Raw, APIKeyPrefix)
	}
	if APIKeyPrefix != "hlmr_sk_" {
		t.Fatalf("APIKeyPrefix = %q, want hlmr_sk_", APIKeyPrefix)
	}
}

func TestKeyPrefixUsesAPIKeyPrefixLength(t *testing.T) {
	raw := APIKeyPrefix + "abcdefghijklmnop"
	if got, want := KeyPrefix(raw), APIKeyPrefix+"abcdefgh"; got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
}
