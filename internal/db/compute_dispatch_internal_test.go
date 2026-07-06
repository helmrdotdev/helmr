package db

import (
	"strings"
	"testing"
)

func TestRenewRunLeaseDoesNotAdvanceDispatchGeneration(t *testing.T) {
	if strings.Contains(renewRunLease, "dispatch_generation = dispatch_generation + 1") {
		t.Fatal("run lease renewal must not advance dispatch_generation; generation fences ownership changes, not lease heartbeats")
	}
}
