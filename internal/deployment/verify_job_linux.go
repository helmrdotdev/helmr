//go:build linux

package deployment

func (job verifierJob) artifactCount() int {
	switch job {
	case programVerifierJob:
		return 2
	case runtimeVerifierJob:
		return 1
	default:
		return 0
	}
}

func (job verifierJob) invalidDiagnostic() string {
	switch job {
	case programVerifierJob:
		return "program is invalid"
	case runtimeVerifierJob:
		return "runtime is invalid"
	default:
		return "artifact is invalid"
	}
}

func (job verifierJob) failedDiagnostic() string {
	switch job {
	case programVerifierJob:
		return "program verifier infrastructure failure"
	case runtimeVerifierJob:
		return "runtime verifier infrastructure failure"
	default:
		return "artifact verifier infrastructure failure"
	}
}
