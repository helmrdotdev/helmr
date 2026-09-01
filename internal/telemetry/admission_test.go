package telemetry

import (
	"bytes"
	"testing"
)

func TestAdmissionLimits(t *testing.T) {
	if err := ValidateEvent("ok", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEvent("ok", bytes.Repeat([]byte{'x'}, MaxEventPayloadBytes+1)); err == nil {
		t.Fatal("expected oversized event payload rejection")
	}
	if err := ValidateRunLog(make([]byte, MaxRunLogContentBytes+1)); err == nil {
		t.Fatal("expected oversized run log rejection")
	}
}
