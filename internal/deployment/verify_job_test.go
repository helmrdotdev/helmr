package deployment

import (
	"errors"
	"testing"

	"github.com/helmrdotdev/helmr/internal/api"
)

func TestProgramVerifierErrorsPreserveDeliveryBoundary(t *testing.T) {
	var deliveryFailure interface {
		DeploymentBuildDeliveryFailureReason() api.WorkerDeploymentBuildDeliveryFailureReason
	}
	if !errors.As(programDeliveryFailure(errors.New("infrastructure")), &deliveryFailure) {
		t.Fatal("program delivery failure was not classified")
	}
	if got := deliveryFailure.DeploymentBuildDeliveryFailureReason(); got != api.WorkerDeploymentBuildDeliveryProgramVerifierFailed {
		t.Fatalf("delivery failure reason = %q", got)
	}
	if errors.As(&verifierInvalidError{diagnostic: "invalid program"}, &deliveryFailure) {
		t.Fatal("deterministic program invalidity was classified as delivery failure")
	}
}

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
