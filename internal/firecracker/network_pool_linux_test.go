//go:build linux

package firecracker

import (
	"net/netip"
	"testing"
)

func TestNetworkPoolRouteConflictTreatsDefaultAsFallback(t *testing.T) {
	pool := netip.MustParsePrefix("198.18.0.0/24")
	if networkPoolRouteConflict(pool, netip.MustParsePrefix("0.0.0.0/0")) {
		t.Fatal("IPv4 default route was treated as a pool conflict")
	}
	if !networkPoolRouteConflict(pool, netip.MustParsePrefix("198.18.0.0/15")) {
		t.Fatal("overlapping non-default route was not treated as a pool conflict")
	}
	if networkPoolRouteConflict(pool, netip.MustParsePrefix("203.0.113.0/24")) {
		t.Fatal("disjoint non-default route was treated as a pool conflict")
	}
}
