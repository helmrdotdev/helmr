package deployment

import "fmt"

type verifierJob string

const (
	programVerifierJob      verifierJob = "program"
	runtimeVerifierJob      verifierJob = "runtime"
	runtimeConformanceJob   verifierJob = "runtime-conformance"
	managerConformanceJob   verifierJob = "manager-conformance"
	toolchainConformanceJob verifierJob = "toolchain-conformance"
)

func parseVerifierJob(value string) (verifierJob, error) {
	job := verifierJob(value)
	switch job {
	case programVerifierJob, runtimeVerifierJob,
		runtimeConformanceJob, managerConformanceJob, toolchainConformanceJob:
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
	case runtimeConformanceJob, managerConformanceJob, toolchainConformanceJob:
		return maxPlatformArtifactDocumentBytes
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
