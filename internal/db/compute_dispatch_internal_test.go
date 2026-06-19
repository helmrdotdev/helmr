package db

import (
	"strings"
	"testing"
)

func TestRenewRunQueueReservationDoesNotAdvanceDispatchGeneration(t *testing.T) {
	if strings.Contains(renewRunQueueReservation, "dispatch_generation = dispatch_generation + 1") {
		t.Fatal("reservation renewal must not advance dispatch_generation; generation fences ownership changes, not lease heartbeats")
	}
}
