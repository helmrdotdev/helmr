package api

import "testing"

func TestParseDurationMillisecondsUsesExactPublicGrammar(t *testing.T) {
	for raw, want := range map[string]int64{
		"1ms": 1,
		"90s": 90_000,
		"15m": 900_000,
		"2h":  7_200_000,
		"7d":  604_800_000,
	} {
		got, err := ParseDurationMilliseconds(raw, "duration", 1, 365*24*60*60*1000)
		if err != nil {
			t.Fatalf("ParseDurationMilliseconds(%q): %v", raw, err)
		}
		if got != want {
			t.Fatalf("ParseDurationMilliseconds(%q) = %d, want %d", raw, got, want)
		}
	}
	for _, raw := range []string{
		"0s", "01s", "+1s", "-1s", " 1s", "1s ", "1.5s", "1h30m", "1", "1ns",
		"999999999999999999999999999999999999d",
	} {
		if _, err := ParseDurationMilliseconds(raw, "duration", 1, 365*24*60*60*1000); err == nil {
			t.Fatalf("ParseDurationMilliseconds(%q) succeeded", raw)
		}
	}
}
