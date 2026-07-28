package deployment

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestBuildGuestErrorsPreserveDeliveryBoundary(t *testing.T) {
	cause := errors.New("guest failed")
	err := buildGuestDeliveryFailure(cause)
	var deliveryFailure interface {
		DeploymentBuildDeliveryFailureReason() api.WorkerDeploymentBuildDeliveryFailureReason
	}
	if !errors.As(err, &deliveryFailure) {
		t.Fatal("build guest delivery failure was not classified")
	}
	if got := deliveryFailure.DeploymentBuildDeliveryFailureReason(); got != api.WorkerDeploymentBuildDeliveryBuildGuestFailed {
		t.Fatalf("delivery failure reason = %q", got)
	}
	if !errors.Is(err, cause) {
		t.Fatal("build guest delivery failure lost its cause")
	}
}
