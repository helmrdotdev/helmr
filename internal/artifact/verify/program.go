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
) (artifact.ProgramMetadata, error) {
	if ctx == nil {
		return artifact.ProgramMetadata{}, errors.New("program verification context is nil")
	}
	if programSnapshot == nil {
		return artifact.ProgramMetadata{}, errors.New("program artifact snapshot is closed")
	}
	file, err := programSnapshot.VerifierFile()
	if err != nil {
		return artifact.ProgramMetadata{}, err
	}
	result, err := runVerifierProcess(ctx, verifierProcessConfig{
		job:            programVerifierJob,
		unitCgroupRoot: unitCgroupRoot,
		leaseIdentity:  leaseIdentity,
		artifacts:      []*os.File{file},
	})
	if err != nil {
		return artifact.ProgramMetadata{}, err
	}
	switch result.kind {
	case verifierVerified:
		verified, err := parseProgramVerification(result.payload)
		if err != nil {
			return artifact.ProgramMetadata{}, fmt.Errorf("parse verified program metadata: %w", err)
		}
		return verified.Metadata, nil
	case verifierInvalid:
		return artifact.ProgramMetadata{}, &verifierInvalidError{diagnostic: result.diagnostic}
	case verifierFailed:
		return artifact.ProgramMetadata{}, errors.New("program verifier failed")
	default:
		return artifact.ProgramMetadata{}, fmt.Errorf(
			"program verifier returned unknown outcome %d",
			result.kind,
		)
	}
}
