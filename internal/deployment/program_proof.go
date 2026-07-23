package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	ProgramProofFormatVersion = 0
	ProgramProofSucceeded     = ProgramProofOutcome("succeeded")
	ProgramProofFailed        = ProgramProofOutcome("failed")
	ProgramProofFailureReason = "program_invalid"

	maxProgramProofBytes = 16 << 20
)

type ProgramProofOutcome string

type ProgramProofResult struct {
	FormatVersion int                  `json:"formatVersion"`
	Outcome       ProgramProofOutcome  `json:"outcome"`
	Declarations  []ProgramDeclaration `json:"declarations,omitempty"`
	Error         *ProgramProofError   `json:"error,omitempty"`
}

type ProgramProofError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func ReadProgramProofFrame(reader io.Reader) (ProgramProofResult, error) {
	raw, err := frameio.ReadMessageFrameBounded(reader, maxProgramProofBytes)
	if err != nil {
		return ProgramProofResult{}, fmt.Errorf("read Program proof frame: %w", err)
	}
	var trailing [1]byte
	if _, err := io.ReadFull(reader, trailing[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			return ProgramProofResult{}, errors.New("Program proof channel contains trailing data")
		}
		return ProgramProofResult{}, fmt.Errorf("check Program proof trailing data: %w", err)
	}
	return ParseProgramProofResult(raw)
}

func ParseProgramProofResult(raw []byte) (ProgramProofResult, error) {
	if len(raw) == 0 || len(raw) > maxProgramProofBytes {
		return ProgramProofResult{}, fmt.Errorf(
			"Program proof size is outside [1,%d]",
			maxProgramProofBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramProofResult{}, fmt.Errorf("canonicalize Program proof: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramProofResult{}, errors.New("Program proof is not RFC 8785 canonical JSON")
	}
	var result ProgramProofResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ProgramProofResult{}, fmt.Errorf("decode Program proof: %w", err)
	}
	if err := ensureEOF(decoder, "Program proof"); err != nil {
		return ProgramProofResult{}, err
	}
	if err := ValidateProgramProofResult(result); err != nil {
		return ProgramProofResult{}, err
	}
	complete, err := CanonicalProgramProofResult(result)
	if err != nil {
		return ProgramProofResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramProofResult{}, errors.New(
			"Program proof does not match the complete canonical v0 shape",
		)
	}
	return cloneProgramProofResult(result), nil
}

func CanonicalProgramProofResult(result ProgramProofResult) ([]byte, error) {
	if err := ValidateProgramProofResult(result); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode Program proof: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize Program proof: %w", err)
	}
	if len(canonical) > maxProgramProofBytes {
		return nil, fmt.Errorf("Program proof size exceeds %d", maxProgramProofBytes)
	}
	return canonical, nil
}

func ValidateProgramProofResult(result ProgramProofResult) error {
	if result.FormatVersion != ProgramProofFormatVersion {
		return fmt.Errorf(
			"Program proof formatVersion = %d, want %d",
			result.FormatVersion,
			ProgramProofFormatVersion,
		)
	}
	switch result.Outcome {
	case ProgramProofSucceeded:
		if result.Error != nil {
			return errors.New("successful Program proof forbids error")
		}
		if len(result.Declarations) == 0 {
			return errors.New("successful Program proof requires declarations")
		}
		for index, declaration := range result.Declarations {
			if err := validateDeclaration(declaration); err != nil {
				return fmt.Errorf("Program proof declaration %d: %w", index, err)
			}
			if index > 0 &&
				compareDeclarations(result.Declarations[index-1], declaration) >= 0 {
				return fmt.Errorf(
					"Program proof declarations are not in canonical order at position %d",
					index,
				)
			}
		}
	case ProgramProofFailed:
		if result.Declarations != nil {
			return errors.New("failed Program proof forbids declarations")
		}
		if result.Error == nil {
			return errors.New("failed Program proof requires error")
		}
		if result.Error.Reason != ProgramProofFailureReason {
			return fmt.Errorf(
				"Program proof failure reason = %q, want %q",
				result.Error.Reason,
				ProgramProofFailureReason,
			)
		}
		if !utf8.ValidString(result.Error.Message) ||
			len(result.Error.Message) > maxBuildFailureMessageBytes ||
			strings.TrimSpace(result.Error.Message) == "" {
			return fmt.Errorf(
				"Program proof failure message must be nonblank UTF-8 of at most %d bytes",
				maxBuildFailureMessageBytes,
			)
		}
	default:
		return fmt.Errorf("Program proof outcome %q is unsupported", result.Outcome)
	}
	return nil
}

func ValidateProgramProof(result ProgramProofResult, index ProgramIndex) error {
	if err := ValidateProgramProofResult(result); err != nil {
		return err
	}
	if result.Outcome != ProgramProofSucceeded {
		return errors.New("Program proof did not succeed")
	}
	if err := ValidateProgramIndex(index); err != nil {
		return err
	}
	if len(result.Declarations) != len(index.Declarations) {
		return errors.New("Program proof declarations do not match Program index")
	}
	for position := range index.Declarations {
		if !sameProgramDeclaration(result.Declarations[position], index.Declarations[position]) {
			return fmt.Errorf(
				"Program proof declaration %d does not match Program index",
				position,
			)
		}
	}
	return nil
}

func sameProgramDeclaration(left, right ProgramDeclaration) bool {
	if left.Kind != right.Kind ||
		left.DeclaredID != right.DeclaredID ||
		len(left.Slots) != len(right.Slots) {
		return false
	}
	for index := range left.Slots {
		if left.Slots[index] != right.Slots[index] {
			return false
		}
	}
	return true
}

func cloneProgramProofResult(result ProgramProofResult) ProgramProofResult {
	declarations := make([]ProgramDeclaration, len(result.Declarations))
	for index := range result.Declarations {
		declarations[index] = result.Declarations[index]
		declarations[index].Slots = append(
			[]DeclarationSlot(nil),
			result.Declarations[index].Slots...,
		)
	}
	result.Declarations = declarations
	if result.Error != nil {
		value := *result.Error
		result.Error = &value
	}
	return result
}
