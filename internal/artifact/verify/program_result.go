package verify

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const programVerificationVersion = 0

type programVerification struct {
	FormatVersion int                   `json:"formatVersion"`
	Index         artifact.ProgramIndex `json:"index"`
}

func parseProgramVerification(raw []byte) (programVerification, error) {
	if len(raw) == 0 || len(raw) > artifact.MaxProgramVerificationSizeBytes {
		return programVerification{}, fmt.Errorf(
			"program verification size is outside [1,%d]",
			artifact.MaxProgramVerificationSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return programVerification{}, fmt.Errorf("canonicalize program verification: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return programVerification{}, errors.New("program verification is not RFC 8785 canonical JSON")
	}
	var verified programVerification
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&verified); err != nil {
		return programVerification{}, fmt.Errorf("decode program verification: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "program verification"); err != nil {
		return programVerification{}, err
	}
	if err := validateProgramVerification(verified); err != nil {
		return programVerification{}, err
	}
	complete, err := canonicalProgramVerification(verified)
	if err != nil {
		return programVerification{}, err
	}
	if !bytes.Equal(raw, complete) {
		return programVerification{}, errors.New(
			"program verification does not match the complete canonical v0 shape",
		)
	}
	verified.Index = verified.Index.Clone()
	return verified, nil
}

func canonicalProgramVerification(verified programVerification) ([]byte, error) {
	if err := validateProgramVerification(verified); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(verified)
	if err != nil {
		return nil, fmt.Errorf("encode program verification: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize program verification: %w", err)
	}
	if len(canonical) > artifact.MaxProgramVerificationSizeBytes {
		return nil, fmt.Errorf(
			"program verification size is outside [1,%d]",
			artifact.MaxProgramVerificationSizeBytes,
		)
	}
	return canonical, nil
}

func validateProgramVerification(verified programVerification) error {
	if verified.FormatVersion != programVerificationVersion {
		return fmt.Errorf(
			"program verification formatVersion = %d, want %d",
			verified.FormatVersion,
			programVerificationVersion,
		)
	}
	if err := artifact.ValidateProgramIndex(verified.Index); err != nil {
		return fmt.Errorf("program verification index: %w", err)
	}
	return nil
}
