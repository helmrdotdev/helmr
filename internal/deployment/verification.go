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
	VerificationResultFormatVersion = 0

	VerificationOutcomeSucceeded = VerificationOutcome("succeeded")
	VerificationOutcomeFailed    = VerificationOutcome("failed")

	VerificationFailureReason = "verification_failed"

	VerificationBuildPlanPath    = "helmr/build-plan.json"
	VerificationDeclarationsPath = "helmr/declarations.json"
	VerificationProgramEntryPath = "helmr/entry.mjs"

	maxVerificationResultBytes = 70 << 20
)

type VerificationOutcome string

type VerificationResult struct {
	FormatVersion int                 `json:"-"`
	Outcome       VerificationOutcome `json:"-"`
	Succeeded     *VerificationSucceeded
	Failed        *VerificationFailed
}

type VerificationSucceeded struct {
	Declarations []ProgramDeclaration `json:"declarations"`
	Files        []VerificationFile   `json:"files"`
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

func ReadVerificationResultFrame(reader io.Reader) (VerificationResult, error) {
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
	return ParseVerificationResult(raw)
}

func ParseVerificationResult(raw []byte) (VerificationResult, error) {
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
	if err := ensureEOF(decoder, "verification result"); err != nil {
		return VerificationResult{}, err
	}
	if err := ValidateVerificationResult(result); err != nil {
		return VerificationResult{}, err
	}
	complete, err := CanonicalVerificationResult(result)
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

func CanonicalVerificationResult(result VerificationResult) ([]byte, error) {
	if err := ValidateVerificationResult(result); err != nil {
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

func ValidateVerificationResult(result VerificationResult) error {
	if result.FormatVersion != VerificationResultFormatVersion {
		return fmt.Errorf(
			"verification result formatVersion = %d, want %d",
			result.FormatVersion,
			VerificationResultFormatVersion,
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
			FormatVersion int                  `json:"formatVersion"`
			Outcome       VerificationOutcome  `json:"outcome"`
			Declarations  []ProgramDeclaration `json:"declarations"`
			Files         []VerificationFile   `json:"files"`
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
			FormatVersion int                  `json:"formatVersion"`
			Outcome       VerificationOutcome  `json:"outcome"`
			Declarations  []ProgramDeclaration `json:"declarations"`
			Files         []VerificationFile   `json:"files"`
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
	return ensureEOF(decoder, "verification result")
}

func validateVerificationSucceeded(succeeded VerificationSucceeded) error {
	if succeeded.Files == nil {
		return errors.New("verification result files must be an array")
	}
	if len(succeeded.Files) != 1 && len(succeeded.Files) != 3 {
		return errors.New("verification result files must contain exactly one or three entries")
	}
	if succeeded.Files[0].Path != VerificationBuildPlanPath {
		return fmt.Errorf(
			"verification result files[0].path = %q, want %q",
			succeeded.Files[0].Path,
			VerificationBuildPlanPath,
		)
	}
	plan, err := ParseBuildPlan([]byte(succeeded.Files[0].Content))
	if err != nil {
		return fmt.Errorf("verification result build plan: %w", err)
	}
	declarations := buildPlanProgramDeclarations(plan)
	if len(declarations) == 0 {
		if len(succeeded.Files) != 1 {
			return errors.New(
				"workspace-only verification result must contain only the build plan",
			)
		}
		if succeeded.Declarations == nil || len(succeeded.Declarations) != 0 {
			return errors.New("workspace-only verification result requires empty declarations")
		}
		return nil
	}
	if len(succeeded.Files) != 3 {
		return errors.New(
			"program-backed verification result must contain all generated program files",
		)
	}
	if succeeded.Files[1].Path != VerificationDeclarationsPath {
		return fmt.Errorf(
			"verification result files[1].path = %q, want %q",
			succeeded.Files[1].Path,
			VerificationDeclarationsPath,
		)
	}
	if succeeded.Files[2].Path != VerificationProgramEntryPath {
		return fmt.Errorf(
			"verification result files[2].path = %q, want %q",
			succeeded.Files[2].Path,
			VerificationProgramEntryPath,
		)
	}
	locator, err := ParseDeclarationLocator([]byte(succeeded.Files[1].Content))
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
	if succeeded.Files[2].Content != ProgramEntry {
		return errors.New("verification result program entry does not match fixed v0 bytes")
	}
	return validateVerifiedDeclarations(succeeded.Declarations, declarations)
}

func validateVerificationFailed(failed VerificationFailed) error {
	if failed.Error.Reason != VerificationFailureReason {
		return fmt.Errorf(
			"verification failure reason %q is unsupported",
			failed.Error.Reason,
		)
	}
	if !utf8.ValidString(failed.Error.Message) ||
		len(failed.Error.Message) > maxBuildFailureMessageBytes ||
		strings.TrimSpace(failed.Error.Message) == "" {
		return fmt.Errorf(
			"verification failure message must be nonblank UTF-8 of at most %d bytes",
			maxBuildFailureMessageBytes,
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
	verified []ProgramDeclaration,
	planned []ProgramDeclaration,
) error {
	if verified == nil {
		return errors.New("verification result declarations must be an array")
	}
	if len(verified) != len(planned) {
		return errors.New("verified declarations do not match the build plan")
	}
	for index, declaration := range verified {
		if err := validateDeclaration(declaration); err != nil {
			return fmt.Errorf("verified declaration %d: %w", index, err)
		}
		if index > 0 && compareDeclarations(verified[index-1], declaration) >= 0 {
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

func ValidateVerifiedProgram(
	result VerificationResult,
	index ProgramIndex,
) error {
	if err := ValidateVerificationResult(result); err != nil {
		return err
	}
	if result.Outcome != VerificationOutcomeSucceeded {
		return errors.New("Program verification did not succeed")
	}
	if err := ValidateProgramIndex(index); err != nil {
		return err
	}
	return validateVerifiedDeclarations(
		result.Succeeded.Declarations,
		index.Declarations,
	)
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

func cloneProgramDeclarations(
	source []ProgramDeclaration,
) []ProgramDeclaration {
	cloned := make([]ProgramDeclaration, len(source))
	for index := range source {
		cloned[index] = source[index]
		cloned[index].Slots = append(
			[]DeclarationSlot(nil),
			source[index].Slots...,
		)
	}
	return cloned
}
