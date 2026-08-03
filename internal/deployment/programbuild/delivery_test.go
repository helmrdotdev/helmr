package programbuild

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func TestBuildGuestErrorsPreserveDeliveryBoundary(t *testing.T) {
	cause := errors.New("guest failed")
	err := buildGuestDeliveryFailure(cause)
	var deliveryFailure interface {
		DeploymentBuildDeliveryFailureReason() workerapi.DeploymentBuildDeliveryFailureReason
	}
	if !errors.As(err, &deliveryFailure) {
		t.Fatal("build guest delivery failure was not classified")
	}
	if got := deliveryFailure.DeploymentBuildDeliveryFailureReason(); got != workerapi.DeploymentBuildDeliveryBuildGuestFailed {
		t.Fatalf("delivery failure reason = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("build guest delivery failure lost its cause")
	}
}
