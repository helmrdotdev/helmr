package control

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/db"
)

func TestWaitpointTimeoutRequiresDelayTimeout(t *testing.T) {
	if _, err := waitpointTimeout(db.WaitpointKindDelay, nil); err == nil {
		t.Fatal("delay timeout validation succeeded without timeout")
	}
}

func TestWaitpointTimeoutAllowsNonDelayWithoutTimeout(t *testing.T) {
	timeout, err := waitpointTimeout(db.WaitpointKindManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	if timeout.Valid {
		t.Fatalf("timeout = %+v, want invalid", timeout)
	}
}

func TestWaitpointTimeoutRejectsNonPositiveTimeout(t *testing.T) {
	zero := int32(0)
	if _, err := waitpointTimeout(db.WaitpointKindManual, &zero); err == nil {
		t.Fatal("timeout validation succeeded with zero")
	}
}

func TestWaitpointTimeoutAcceptsPositiveTimeout(t *testing.T) {
	seconds := int32(30)
	timeout, err := waitpointTimeout(db.WaitpointKindDelay, &seconds)
	if err != nil {
		t.Fatal(err)
	}
	if !timeout.Valid || timeout.Int32 != seconds {
		t.Fatalf("timeout = %+v, want %d", timeout, seconds)
	}
}
