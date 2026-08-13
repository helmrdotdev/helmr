package deployment

import "fmt"

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
		return maxProgramVerificationSizeBytes
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
