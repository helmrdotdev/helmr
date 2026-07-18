package deployment

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/api"
)

type verifiedProgramSnapshot struct {
	index     ProgramIndex
	canonical []byte
}

type programInvalidError struct {
	diagnostic string
}

func (err *programInvalidError) Error() string {
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
	code *programSnapshot,
	dependencies *programSnapshot,
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
	result, err := runProgramVerifierProcess(ctx, programVerifierProcessConfig{
		executable:     programVerifierExecutable,
		arguments:      programVerifierChildArguments(),
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		code:           codeFile,
		dependencies:   dependencyFile,
	})
	if err != nil {
		return verifiedProgramSnapshot{}, programDeliveryFailure(err)
	}
	switch result.kind {
	case programVerifierVerified:
		index, err := ParseProgramIndex(result.index)
		if err != nil {
			return verifiedProgramSnapshot{}, programDeliveryFailure(
				fmt.Errorf("parse verified Program index: %w", err),
			)
		}
		return verifiedProgramSnapshot{
			index:     index,
			canonical: append([]byte(nil), result.index...),
		}, nil
	case programVerifierInvalid:
		return verifiedProgramSnapshot{}, &programInvalidError{
			diagnostic: result.diagnostic,
		}
	case programVerifierFailed:
		return verifiedProgramSnapshot{}, programDeliveryFailure(errors.New("program verifier failed"))
	default:
		return verifiedProgramSnapshot{}, programDeliveryFailure(
			fmt.Errorf("program verifier returned unknown outcome %d", result.kind),
		)
	}
}
