package api

import (
	"strings"
	"testing"
)

func TestValidateClientVersion(t *testing.T) {
	for name, value := range map[string]string{
		"empty":     "",
		"opaque":    "0.3.0-dev+local",
		"255 bytes": strings.Repeat("x", MaxClientVersionBytes),
		"utf8":      "開発版",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateClientVersion(value); err != nil {
				t.Fatalf("ValidateClientVersion(%q): %v", value, err)
			}
		})
	}
	for name, value := range map[string]string{
		"256 bytes":       strings.Repeat("x", MaxClientVersionBytes+1),
		"multibyte":       strings.Repeat("x", MaxClientVersionBytes-1) + "界",
		"leading":         " v1",
		"trailing":        "v1 ",
		"control":         "v1\n",
		"unicode control": "v1\u0085dev",
		"invalid utf8":    string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateClientVersion(value); err == nil {
				t.Fatalf("ValidateClientVersion(%q) returned nil", value)
			}
		})
	}
}
