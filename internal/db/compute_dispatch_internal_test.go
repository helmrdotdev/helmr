package db

import (
	"strings"
	"testing"
)

func TestValidateRunLeaseDispatchRenewalDoesNotAdvanceDispatchGeneration(t *testing.T) {
	if strings.Contains(validateRunLeaseDispatchRenewal, "dispatch_generation = dispatch_generation + 1") {
		t.Fatal("dispatch lease renewal validation must not advance dispatch_generation; generation fences ownership changes, not lease heartbeats")
	}
}
