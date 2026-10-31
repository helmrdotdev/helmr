package auth

import (
	"testing"
	"time"
)

func TestProvedRecentlyBoundaries(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		at   time.Time
		want bool
	}{
		{"exactly one hour old", now.Add(-RecentProofWindow), true},
		{"just over one hour old", now.Add(-RecentProofWindow - time.Nanosecond), false},
		{"thirty seconds in the future", now.Add(30 * time.Second), true},
		{"two minutes in the future", now.Add(2 * time.Minute), false},
		{"never proved", time.Time{}, false},
	} {
		if got := ProvedRecently(tc.at, now); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
