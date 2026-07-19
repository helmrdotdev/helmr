package deployment

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
)

type verifierJob string

const (
	programVerifierJob verifierJob = "program"
	runtimeVerifierJob verifierJob = "runtime"
)

func parseVerifierJob(value string) (verifierJob, error) {
	job := verifierJob(value)
	switch job {
	case programVerifierJob, runtimeVerifierJob:
		return job, nil
	default:
		return "", fmt.Errorf("artifact verifier job = %q", value)
	}
}

func (job verifierJob) verifiedPayloadLimit() int64 {
	switch job {
	case programVerifierJob:
		return maxProgramReceiptSizeBytes
	case runtimeVerifierJob:
		return maxRuntimeDocumentBytes
	default:
		return 0
	}
}

type verifierInvalidError struct {
	diagnostic string
}

func (err *verifierInvalidError) Error() string {
	return err.diagnostic
}

type buildDeliveryError struct {
	reason api.WorkerDeploymentBuildDeliveryFailureReason
	err    error
}

func (err *buildDeliveryError) Error() string {
	return err.err.Error()
}

func (err *buildDeliveryError) Unwrap() error {
	return err.err
}

func (err *buildDeliveryError) DeploymentBuildDeliveryFailureReason() api.WorkerDeploymentBuildDeliveryFailureReason {
	return err.reason
}

func programDeliveryFailure(err error) error {
	return &buildDeliveryError{reason: api.WorkerDeploymentBuildDeliveryProgramVerifierFailed, err: err}
}

func buildGuestDeliveryFailure(err error) error {
	return &buildDeliveryError{reason: api.WorkerDeploymentBuildDeliveryBuildGuestFailed, err: err}
}
