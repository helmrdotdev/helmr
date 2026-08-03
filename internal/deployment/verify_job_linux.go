//go:build linux

package deployment

func (job verifierJob) conformance() bool {
	switch job {
	case runtimeConformanceJob, managerConformanceJob, toolchainConformanceJob:
		return true
	default:
		return false
	}
}

func (job verifierJob) artifactCount() int {
	switch job {
	case programVerifierJob:
		return 2
	case runtimeVerifierJob:
		return 1
	case runtimeConformanceJob:
		return 2
	case managerConformanceJob, toolchainConformanceJob:
		return 3
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
	case runtimeConformanceJob:
		return "runtime conformance failed"
	case managerConformanceJob:
		return "Manager conformance failed"
	case toolchainConformanceJob:
		return "toolchain conformance failed"
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
	case runtimeConformanceJob, managerConformanceJob, toolchainConformanceJob:
		return "Platform conformance validator infrastructure failure"
	default:
		return "artifact verifier infrastructure failure"
	}
}
