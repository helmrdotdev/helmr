package run

import "testing"

func TestProtocolTimingIsCoherent(t *testing.T) {
	if ReservationTTL <= 0 || StartDeadline <= 0 || LeaseTTL <= 0 || FinalizationTTL <= 0 {
		t.Fatal("run protocol durations must be positive")
	}
	if StartDeadline > LeaseTTL {
		t.Fatalf("start deadline %s exceeds lease TTL %s", StartDeadline, LeaseTTL)
	}
}
