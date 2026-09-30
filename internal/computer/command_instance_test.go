package computer

import (
	"testing"
	"uuid"
)

// A CommandInstance whose Command is not bound, including the zero value,
// grants nothing and records no activity.
func TestUnboundCommandInstanceGrantsNothing(t *testing.T) {
	var unbound CommandInstance
	if unbound.Bound() || unbound.Serving() || unbound.On(Host{}, uuid.Nil(), 0) {
		t.Fatal("unbound CommandInstance reports an Instance")
	}
	if err := unbound.TouchActivity(t.Context(), uuid.NewV7(), uuid.NewV7()); err != nil {
		t.Fatalf("TouchActivity on unbound CommandInstance = %v", err)
	}
}
