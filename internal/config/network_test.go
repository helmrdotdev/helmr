package config

import "testing"

func TestParseCanonicalBlockedIPv4Prefixes(t *testing.T) {
	prefixes, err := parseCanonicalBlockedIPv4Prefixes(`[
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"169.254.0.0/16"
	]`)
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) != 4 || prefixes[2].String() != "100.64.0.0/10" {
		t.Fatalf("prefixes = %v", prefixes)
	}
	if empty, err := parseCanonicalBlockedIPv4Prefixes(`[]`); err != nil || len(empty) != 0 {
		t.Fatalf("explicit empty prefixes = %v, %v", empty, err)
	}
}

func TestParseCanonicalBlockedIPv4PrefixesRejectsAmbiguousInput(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `{}`, `[] trailing`, `["fc00::/7"]`,
		`["10.0.0.1/8"]`, `["10.0.0.0/08"]`, `[" 10.0.0.0/8"]`,
		`["10.0.0.0/8","0.0.0.0/8"]`,
		`["10.0.0.0/8","10.0.0.0/8"]`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseCanonicalBlockedIPv4Prefixes(raw); err == nil {
				t.Fatalf("accepted %q", raw)
			}
		})
	}
}
