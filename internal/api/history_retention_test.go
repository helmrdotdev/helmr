package api

import "testing"

func TestHistoryRetentionDuration(t *testing.T) {
	for _, tc := range []struct{ input, want string }{{"never", "never"}, {"30d", "30d"}, {"48h", "2d"}, {"90m", "90m"}, {"60s", "1m"}, {"3601s", "3601s"}, {"9223372036854775807s", "9223372036854775807s"}} {
		seconds, err := ParseHistoryRetention(tc.input)
		if err != nil || FormatHistoryRetention(seconds) != tc.want {
			t.Fatalf("%s: %v %v", tc.input, seconds, err)
		}
	}
	for _, input := range []string{"", "0s", "-1d", "1.5h", "1ms", "01d", " 30d", "30d ", "Never", "9223372036854775808s", "9223372036854775807d"} {
		if _, err := ParseHistoryRetention(input); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
