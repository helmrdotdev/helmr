package datapath

import (
	"net/netip"
	"slices"
	"testing"
)

func TestManagedCloudPublicIPv4DenyPrefixes(t *testing.T) {
	want := []string{
		"0.0.0.0/8",
		"10.0.0.0/8",
		"100.64.0.0/10",
		"127.0.0.0/8",
		"169.254.0.0/16",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"224.0.0.0/4",
		"240.0.0.0/4",
	}

	prefixes := ManagedCloudPublicIPv4DenyPrefixes()
	got := make([]string, len(prefixes))
	for i, prefix := range prefixes {
		if !prefix.Addr().Is4() {
			t.Fatalf("prefix %q is not IPv4", prefix)
		}
		if prefix != prefix.Masked() {
			t.Fatalf("prefix %q is not canonical", prefix)
		}
		got[i] = prefix.String()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("deny prefixes = %v, want %v", got, want)
	}

	prefixes[0] = netip.MustParsePrefix("8.8.8.0/24")
	if got := ManagedCloudPublicIPv4DenyPrefixes()[0].String(); got != want[0] {
		t.Fatalf("caller mutated immutable prefixes: first prefix = %q", got)
	}
}
