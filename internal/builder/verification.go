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

	verificationBuildPlanPath       = "helmr/build-plan.json"
	verificationDefinitionIndexPath = "helmr/definition-index.json"

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

// DefinitionIndex returns the sole runtime export index from successful verification.
func (result VerificationResult) DefinitionIndex() []byte {
	return []byte(result.Succeeded.Files[1].Content)
}

type VerificationSucceeded struct {
	Files []VerificationFile `json:"files"`
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
			FormatVersion int                 `json:"formatVersion"`
			Outcome       VerificationOutcome `json:"outcome"`
			Files         []VerificationFile  `json:"files"`
		}{
			FormatVersion: result.FormatVersion,
			Outcome:       result.Outcome,
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
			FormatVersion int                 `json:"formatVersion"`
			Outcome       VerificationOutcome `json:"outcome"`
			Files         []VerificationFile  `json:"files"`
		}
		if err := decodeClosedVerificationResult(raw, &wire); err != nil {
			return err
		}
		result.Succeeded = &VerificationSucceeded{
			Files: wire.Files,
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
	if len(succeeded.Files) != 2 || succeeded.Files[0].Path != verificationBuildPlanPath || succeeded.Files[1].Path != verificationDefinitionIndexPath {
		return errors.New("verification requires exactly build-plan.json and definition-index.json in that order")
	}
	plan, err := definition.ParseBuildPlan([]byte(succeeded.Files[0].Content))
	if err != nil {
		return fmt.Errorf("verification build plan: %w", err)
	}
	index, err := artifact.ParseDefinitionIndex([]byte(succeeded.Files[1].Content))
	if err != nil {
		return fmt.Errorf("verification Definition index: %w", err)
	}
	return artifact.ValidateBuildPlanDefinitionIndex(plan, index)
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
		result.Succeeded = &VerificationSucceeded{
			Files: files,
		}
	}
	if result.Failed != nil {
		failed := *result.Failed
		result.Failed = &failed
	}
	return result
}

func validateVerifiedProgram(result VerificationResult, index artifact.ProgramMetadata) error {
	if err := validateVerificationResult(result); err != nil {
		return err
	}
	if result.Outcome != VerificationOutcomeSucceeded {
		return errors.New("program verification did not succeed")
	}
	if err := artifact.ValidateProgramMetadata(index); err != nil {
		return err
	}
	runtimeIndex, err := artifact.ParseDefinitionIndex(result.DefinitionIndex())
	if err != nil {
		return err
	}
	return artifact.ValidateProgramDefinitionIndex(index, runtimeIndex)
}
