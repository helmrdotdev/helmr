package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/frameio"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	verificationResultFormatVersion = 0

	VerificationOutcomeSucceeded = VerificationOutcome("succeeded")
	VerificationOutcomeFailed    = VerificationOutcome("failed")

	verificationFailureReason = "verification_failed"

	verificationBuildPlanPath    = "helmr/build-plan.json"
	verificationDeclarationsPath = "helmr/analysis-locators.json"

	maxVerificationResultBytes         = 70 << 20
	maxVerificationFailureMessageBytes = 16 << 10
)

type VerificationOutcome string

type VerificationResult struct {
	FormatVersion int                 `json:"-"`
	Outcome       VerificationOutcome `json:"-"`
	Succeeded     *VerificationSucceeded
	Failed        *VerificationFailed
}

// BuildPlan returns the build plan document of a succeeded verification
// result, which is always its first file (verificationBuildPlanPath).
func (result VerificationResult) BuildPlan() []byte {
	return []byte(result.Succeeded.Files[0].Content)
}

// Declarations returns the declaration locator document of a program-backed
// succeeded verification result, which is always its second file
// (verificationDeclarationsPath).
func (result VerificationResult) Declarations() []byte {
	return []byte(result.Succeeded.Files[1].Content)
}

type VerificationSucceeded struct {
	Declarations []artifact.ProgramDeclaration `json:"declarations"`
	Files        []VerificationFile            `json:"files"`
}

type VerificationFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type VerificationFailed struct {
	Error VerificationError `json:"error"`
}

type VerificationError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func readVerificationResultFrame(reader io.Reader) (VerificationResult, error) {
	raw, err := frameio.ReadMessageFrameBounded(reader, maxVerificationResultBytes)
	if err != nil {
		return VerificationResult{}, fmt.Errorf("read verification result frame: %w", err)
	}
	var trailing [1]byte
	if _, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		if err == nil {
			return VerificationResult{}, errors.New(
				"verification result channel contains trailing data",
			)
		}
		return VerificationResult{}, fmt.Errorf(
			"check verification result channel trailing data: %w",
			err,
		)
	}
	return parseVerificationResult(raw)
}

func parseVerificationResult(raw []byte) (VerificationResult, error) {
	if len(raw) == 0 || len(raw) > maxVerificationResultBytes {
		return VerificationResult{}, fmt.Errorf(
			"verification result size is outside [1,%d]",
			maxVerificationResultBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return VerificationResult{}, fmt.Errorf("canonicalize verification result: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return VerificationResult{}, errors.New(
			"verification result is not RFC 8785 canonical JSON",
		)
	}

	var result VerificationResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return VerificationResult{}, fmt.Errorf("decode verification result: %w", err)
	}
	if err := jsoncanon.RequireEOF(decoder, "verification result"); err != nil {
		return VerificationResult{}, err
	}
	if err := validateVerificationResult(result); err != nil {
		return VerificationResult{}, err
	}
	complete, err := canonicalVerificationResult(result)
	if err != nil {
		return VerificationResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return VerificationResult{}, errors.New(
			"verification result does not match the complete canonical v0 shape",
		)
	}
	return cloneVerificationResult(result), nil
}

func canonicalVerificationResult(result VerificationResult) ([]byte, error) {
	if err := validateVerificationResult(result); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode verification result: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize verification result: %w", err)
	}
	if len(canonical) > maxVerificationResultBytes {
		return nil, fmt.Errorf(
			"verification result size is outside [1,%d]",
			maxVerificationResultBytes,
		)
	}
	return canonical, nil
}

func validateVerificationResult(result VerificationResult) error {
	if result.FormatVersion != verificationResultFormatVersion {
		return fmt.Errorf(
			"verification result formatVersion = %d, want %d",
			result.FormatVersion,
			verificationResultFormatVersion,
		)
	}
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return errors.New("verification result must contain exactly one outcome value")
	}
	switch result.Outcome {
	case VerificationOutcomeSucceeded:
		if result.Succeeded == nil {
			return errors.New("succeeded verification result requires success data")
		}
		return validateVerificationSucceeded(*result.Succeeded)
	case VerificationOutcomeFailed:
		if result.Failed == nil {
			return errors.New("failed verification result requires failure data")
		}
		return validateVerificationFailed(*result.Failed)
	default:
		return fmt.Errorf("verification result outcome %q is unsupported", result.Outcome)
	}
}

func (result VerificationResult) MarshalJSON() ([]byte, error) {
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return nil, errors.New("verification result must contain exactly one outcome value")
	}
	switch result.Outcome {
	case VerificationOutcomeSucceeded:
		if result.Succeeded == nil {
			return nil, errors.New("succeeded verification result requires success data")
		}
		return json.Marshal(struct {
			FormatVersion int                           `json:"formatVersion"`
			Outcome       VerificationOutcome           `json:"outcome"`
			Declarations  []artifact.ProgramDeclaration `json:"declarations"`
			Files         []VerificationFile            `json:"files"`
		}{
			FormatVersion: result.FormatVersion,
			Outcome:       result.Outcome,
			Declarations:  result.Succeeded.Declarations,
			Files:         result.Succeeded.Files,
		})
	case VerificationOutcomeFailed:
		if result.Failed == nil {
			return nil, errors.New("failed verification result requires failure data")
		}
		return json.Marshal(struct {
			FormatVersion int                 `json:"formatVersion"`
			Outcome       VerificationOutcome `json:"outcome"`
			Error         VerificationError   `json:"error"`
		}{
			FormatVersion: result.FormatVersion,
			Outcome:       result.Outcome,
			Error:         result.Failed.Error,
		})
	default:
		return nil, fmt.Errorf("verification result outcome %q is unsupported", result.Outcome)
	}
}

func (result *VerificationResult) UnmarshalJSON(raw []byte) error {
	var header struct {
		FormatVersion int                 `json:"formatVersion"`
		Outcome       VerificationOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	*result = VerificationResult{
		FormatVersion: header.FormatVersion,
		Outcome:       header.Outcome,
	}
	switch header.Outcome {
	case VerificationOutcomeSucceeded:
		var wire struct {
			FormatVersion int                           `json:"formatVersion"`
			Outcome       VerificationOutcome           `json:"outcome"`
			Declarations  []artifact.ProgramDeclaration `json:"declarations"`
			Files         []VerificationFile            `json:"files"`
		}
		if err := decodeClosedVerificationResult(raw, &wire); err != nil {
			return err
		}
		result.Succeeded = &VerificationSucceeded{
			Declarations: wire.Declarations,
			Files:        wire.Files,
		}
	case VerificationOutcomeFailed:
		var wire struct {
			FormatVersion int                 `json:"formatVersion"`
			Outcome       VerificationOutcome `json:"outcome"`
			Error         VerificationError   `json:"error"`
		}
		if err := decodeClosedVerificationResult(raw, &wire); err != nil {
			return err
		}
		result.Failed = &VerificationFailed{Error: wire.Error}
	default:
		return fmt.Errorf("verification result outcome %q is unsupported", header.Outcome)
	}
	return nil
}

func decodeClosedVerificationResult(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return jsoncanon.RequireEOF(decoder, "verification result")
}

func validateVerificationSucceeded(succeeded VerificationSucceeded) error {
	if succeeded.Files == nil {
		return errors.New("verification result files must be an array")
	}
	if len(succeeded.Files) != 1 && len(succeeded.Files) != 2 {
		return errors.New("verification result files must contain exactly one or two entries")
	}
	if succeeded.Files[0].Path != verificationBuildPlanPath {
		return fmt.Errorf(
			"verification result files[0].path = %q, want %q",
			succeeded.Files[0].Path,
			verificationBuildPlanPath,
		)
	}
	plan, err := definition.ParseBuildPlan([]byte(succeeded.Files[0].Content))
	if err != nil {
		return fmt.Errorf("verification result build plan: %w", err)
	}
	declarations := artifact.BuildPlanProgramDeclarations(plan)
	if len(declarations) == 0 {
		if len(succeeded.Files) != 1 {
			return errors.New(
				"computer-only verification result must contain only the build plan",
			)
		}
		if succeeded.Declarations == nil || len(succeeded.Declarations) != 0 {
			return errors.New("computer-only verification result requires empty declarations")
		}
		return nil
	}
	if len(succeeded.Files) != 2 {
		return errors.New(
			"program-backed verification result must contain all generated program files",
		)
	}
	if succeeded.Files[1].Path != verificationDeclarationsPath {
		return fmt.Errorf(
			"verification result files[1].path = %q, want %q",
			succeeded.Files[1].Path,
			verificationDeclarationsPath,
		)
	}
	locator, err := artifact.ParseDeclarationLocator([]byte(succeeded.Files[1].Content))
	if err != nil {
		return fmt.Errorf("verification result declaration locator: %w", err)
	}
	if len(locator.Declarations) != len(declarations) {
		return errors.New(
			"verification result declaration locator does not match build plan",
		)
	}
	for index, declaration := range declarations {
		located := locator.Declarations[index]
		if located.Kind != declaration.Kind ||
			located.DeclaredID != declaration.DeclaredID {
			return fmt.Errorf(
				"verification result declaration locator does not match build plan at position %d",
				index,
			)
		}
	}
	return validateVerifiedDeclarations(succeeded.Declarations, declarations)
}

func validateVerificationFailed(failed VerificationFailed) error {
	if failed.Error.Reason != verificationFailureReason {
		return fmt.Errorf(
			"verification failure reason %q is unsupported",
			failed.Error.Reason,
		)
	}
	if !utf8.ValidString(failed.Error.Message) ||
		len(failed.Error.Message) > maxVerificationFailureMessageBytes ||
		strings.TrimSpace(failed.Error.Message) == "" {
		return fmt.Errorf(
			"verification failure message must be nonblank UTF-8 of at most %d bytes",
			maxVerificationFailureMessageBytes,
		)
	}
	return nil
}

func cloneVerificationResult(result VerificationResult) VerificationResult {
	if result.Succeeded != nil {
		files := append([]VerificationFile(nil), result.Succeeded.Files...)
		declarations := cloneProgramDeclarations(result.Succeeded.Declarations)
		result.Succeeded = &VerificationSucceeded{
			Declarations: declarations,
			Files:        files,
		}
	}
	if result.Failed != nil {
		failed := *result.Failed
		result.Failed = &failed
	}
	return result
}

func validateVerifiedDeclarations(
	verified []artifact.ProgramDeclaration,
	planned []artifact.ProgramDeclaration,
) error {
	if verified == nil {
		return errors.New("verification result declarations must be an array")
	}
	if len(verified) != len(planned) {
		return errors.New("verified declarations do not match the build plan")
	}
	for index, declaration := range verified {
		if err := artifact.ValidateDeclaration(declaration); err != nil {
			return fmt.Errorf("verified declaration %d: %w", index, err)
		}
		if index > 0 && artifact.CompareDeclarations(verified[index-1], declaration) >= 0 {
			return fmt.Errorf(
				"verified declarations are not in canonical order at position %d",
				index,
			)
		}
		if !sameProgramDeclaration(declaration, planned[index]) {
			return fmt.Errorf(
				"verified declaration %d does not match the build plan",
				index,
			)
		}
	}
	return nil
}

func validateVerifiedProgram(
	result VerificationResult,
	index artifact.ProgramIndex,
) error {
	if err := validateVerificationResult(result); err != nil {
		return err
	}
	if result.Outcome != VerificationOutcomeSucceeded {
		return errors.New("program verification did not succeed")
	}
	if err := artifact.ValidateProgramIndex(index); err != nil {
		return err
	}
	return validateVerifiedDeclarations(
		result.Succeeded.Declarations,
		artifact.ProgramIndexExecutionDeclarations(index),
	)
}

func sameProgramDeclaration(left, right artifact.ProgramDeclaration) bool {
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

func cloneProgramDeclarations(
	source []artifact.ProgramDeclaration,
) []artifact.ProgramDeclaration {
	cloned := make([]artifact.ProgramDeclaration, len(source))
	for index := range source {
		cloned[index] = source[index]
		cloned[index].Slots = append(
			[]artifact.DeclarationSlot(nil),
			source[index].Slots...,
		)
	}
	return cloned
}
