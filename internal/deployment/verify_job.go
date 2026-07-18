package deployment

import (
	"context"
	"errors"
	"fmt"
	"os"

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

func (job verifierJob) verifiedPayloadLimit() int64 {
	switch job {
	case programVerifierJob:
		return maxProgramFileSizeBytes
	case runtimeVerifierJob:
		return maxRuntimeDocumentBytes
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

type verifiedProgramSnapshot struct {
	index     ProgramIndex
	canonical []byte
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

func verifyProgramSnapshots(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	code *artifactSnapshot,
	dependencies *artifactSnapshot,
) (verifiedProgramSnapshot, error) {
	codeFile, err := code.verifierFile()
	if err != nil {
		return verifiedProgramSnapshot{}, programDeliveryFailure(
			fmt.Errorf("open code snapshot for verification: %w", err),
		)
	}
	dependencyFile, err := dependencies.verifierFile()
	if err != nil {
		return verifiedProgramSnapshot{}, programDeliveryFailure(
			fmt.Errorf("open dependency snapshot for verification: %w", err),
		)
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            programVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{codeFile, dependencyFile},
	})
	if err != nil {
		return verifiedProgramSnapshot{}, programDeliveryFailure(err)
	}
	switch result.kind {
	case verifierVerified:
		verified, err := verifiedProgramResult(result.payload)
		if err != nil {
			return verifiedProgramSnapshot{}, programDeliveryFailure(err)
		}
		return verified, nil
	case verifierInvalid:
		return verifiedProgramSnapshot{}, &verifierInvalidError{
			diagnostic: result.diagnostic,
		}
	case verifierFailed:
		return verifiedProgramSnapshot{}, programDeliveryFailure(errors.New("program verifier failed"))
	default:
		return verifiedProgramSnapshot{}, programDeliveryFailure(
			fmt.Errorf("program verifier returned unknown outcome %d", result.kind),
		)
	}
}

func verifiedProgramResult(payload []byte) (verifiedProgramSnapshot, error) {
	index, err := ParseProgramIndex(payload)
	if err != nil {
		return verifiedProgramSnapshot{}, fmt.Errorf("parse verified Program index: %w", err)
	}
	return verifiedProgramSnapshot{
		index:     index,
		canonical: append([]byte(nil), payload...),
	}, nil
}
