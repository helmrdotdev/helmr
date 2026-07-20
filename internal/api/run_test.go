package api

import (
	"strings"
	"testing"
)

func TestValidateConcurrencyKey(t *testing.T) {
	t.Parallel()

	valid := []string{
		"a",
		strings.Repeat("a", 512),
		"\u00a0key\u00a0",
		"日本語",
	}
	for _, key := range valid {
		if err := ValidateConcurrencyKey(key); err != nil {
			t.Errorf("ValidateConcurrencyKey(%q) returned %v", key, err)
		}
	}

	invalid := []string{
		"",
		" key",
		"key ",
		"\tkey",
		"key\n",
		"key\x00value",
		strings.Repeat("a", 513),
		string([]byte{0xff}),
	}
	for _, key := range invalid {
		if err := ValidateConcurrencyKey(key); err == nil {
			t.Errorf("ValidateConcurrencyKey(%q) returned nil", key)
		}
	}
}
