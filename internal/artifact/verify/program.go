package verify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/snapshot"
)

func Program(
	ctx context.Context,
	unitCgroupRoot string,
	leaseIdentity string,
	programSnapshot *snapshot.Program,
) (artifact.ProgramIndex, error) {
	if ctx == nil {
		return artifact.ProgramIndex{}, errors.New("program verification context is nil")
	}
	if programSnapshot == nil {
		return artifact.ProgramIndex{}, errors.New("program artifact snapshot is closed")
	}
	file, err := programSnapshot.VerifierFile()
	if err != nil {
		return artifact.ProgramIndex{}, err
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            programVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{file},
	})
	if err != nil {
		return artifact.ProgramIndex{}, err
	}
	switch result.kind {
	case verifierVerified:
		verified, err := parseProgramVerification(result.payload)
		if err != nil {
			return artifact.ProgramIndex{}, fmt.Errorf("parse verified program index: %w", err)
		}
		return verified.Index, nil
	case verifierInvalid:
		return artifact.ProgramIndex{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return artifact.ProgramIndex{}, errors.New("program verifier failed")
	default:
		return artifact.ProgramIndex{}, fmt.Errorf(
			"program verifier returned unknown outcome %d",
			result.kind,
		)
	}
}
