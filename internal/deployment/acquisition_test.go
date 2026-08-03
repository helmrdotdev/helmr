package deployment

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestConformanceFailureClassifiesOnlyVerifiedInvalidResultAsDeterministic(t *testing.T) {
	invalid := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		nil,
	)
	var deterministic interface {
		PlatformAcquisitionFailureReason() api.WorkerPlatformAcquisitionFailureReason
	}
	if !errors.As(invalid, &deterministic) ||
		deterministic.PlatformAcquisitionFailureReason() !=
			api.WorkerPlatformAcquisitionConformanceFailed {
		t.Fatalf("invalid result classification = %v", invalid)
	}

	infrastructure := conformanceFailure(errors.New("validator unavailable"), nil)
	if errors.As(infrastructure, &deterministic) {
		t.Fatalf("validator outage was terminalized: %v", infrastructure)
	}

	closeFailure := conformanceFailure(
		&verifierInvalidError{diagnostic: "fixture failed"},
		errors.New("snapshot close failed"),
	)
	if errors.As(closeFailure, &deterministic) {
		t.Fatalf("snapshot close failure was terminalized: %v", closeFailure)
	}
}
