package region

import (
	"strings"
	"testing"
)

func TestValidateID(t *testing.T) {
	for name, value := range map[string]string{
		"punctuation": "a:b_c",
		"255 bytes":   strings.Repeat("r", MaxIDBytes),
		"utf8":        "東京",
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateID(value); err != nil {
				t.Fatalf("ValidateID(%q): %v", value, err)
			}
		})
	}
	for name, value := range map[string]string{
		"empty":           "",
		"256 bytes":       strings.Repeat("r", MaxIDBytes+1),
		"multibyte":       strings.Repeat("r", MaxIDBytes-1) + "界",
		"leading":         " region",
		"trailing":        "region ",
		"control":         "region\x00id",
		"unicode control": "region\u0085id",
		"invalid utf8":    string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateID(value); err == nil {
				t.Fatalf("ValidateID(%q) returned nil", value)
			}
		})
	}
}
