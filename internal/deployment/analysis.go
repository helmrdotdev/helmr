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
	AnalysisResultFormatVersion = 0

	AnalysisOutcomeSucceeded = AnalysisOutcome("succeeded")
	AnalysisOutcomeFailed    = AnalysisOutcome("failed")

	AnalysisFailureReason = "analysis_failed"

	AnalysisBuildPlanPath    = "helmr/build-plan.json"
	AnalysisDeclarationsPath = "helmr/declarations.json"
	AnalysisProgramEntryPath = "helmr/entry.mjs"

	maxAnalysisResultBytes = 70 << 20
)

type AnalysisOutcome string

type AnalysisResult struct {
	FormatVersion int             `json:"-"`
	Outcome       AnalysisOutcome `json:"-"`
	Succeeded     *AnalysisSucceeded
	Failed        *AnalysisFailed
}

type AnalysisSucceeded struct {
	Files []AnalysisFile `json:"files"`
}

type AnalysisFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type AnalysisFailed struct {
	Error AnalysisError `json:"error"`
}

type AnalysisError struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func ReadAnalysisResultFrame(reader io.Reader) (AnalysisResult, error) {
	raw, err := frameio.ReadMessageFrameBounded(reader, maxAnalysisResultBytes)
	if err != nil {
		return AnalysisResult{}, fmt.Errorf("read analysis result frame: %w", err)
	}
	var trailing [1]byte
	if _, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
		if err == nil {
			return AnalysisResult{}, errors.New(
				"analysis result channel contains trailing data",
			)
		}
		return AnalysisResult{}, fmt.Errorf(
			"check analysis result channel trailing data: %w",
			err,
		)
	}
	return ParseAnalysisResult(raw)
}

func ParseAnalysisResult(raw []byte) (AnalysisResult, error) {
	if len(raw) == 0 || len(raw) > maxAnalysisResultBytes {
		return AnalysisResult{}, fmt.Errorf(
			"analysis result size is outside [1,%d]",
			maxAnalysisResultBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return AnalysisResult{}, fmt.Errorf("canonicalize analysis result: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return AnalysisResult{}, errors.New(
			"analysis result is not RFC 8785 canonical JSON",
		)
	}

	var result AnalysisResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return AnalysisResult{}, fmt.Errorf("decode analysis result: %w", err)
	}
	if err := ensureEOF(decoder, "analysis result"); err != nil {
		return AnalysisResult{}, err
	}
	if err := ValidateAnalysisResult(result); err != nil {
		return AnalysisResult{}, err
	}
	complete, err := CanonicalAnalysisResult(result)
	if err != nil {
		return AnalysisResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return AnalysisResult{}, errors.New(
			"analysis result does not match the complete canonical v0 shape",
		)
	}
	return cloneAnalysisResult(result), nil
}

func CanonicalAnalysisResult(result AnalysisResult) ([]byte, error) {
	if err := ValidateAnalysisResult(result); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode analysis result: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize analysis result: %w", err)
	}
	if len(canonical) > maxAnalysisResultBytes {
		return nil, fmt.Errorf(
			"analysis result size is outside [1,%d]",
			maxAnalysisResultBytes,
		)
	}
	return canonical, nil
}

func ValidateAnalysisResult(result AnalysisResult) error {
	if result.FormatVersion != AnalysisResultFormatVersion {
		return fmt.Errorf(
			"analysis result formatVersion = %d, want %d",
			result.FormatVersion,
			AnalysisResultFormatVersion,
		)
	}
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return errors.New("analysis result must contain exactly one outcome value")
	}
	switch result.Outcome {
	case AnalysisOutcomeSucceeded:
		if result.Succeeded == nil {
			return errors.New("succeeded analysis result requires success data")
		}
		return validateAnalysisSucceeded(*result.Succeeded)
	case AnalysisOutcomeFailed:
		if result.Failed == nil {
			return errors.New("failed analysis result requires failure data")
		}
		return validateAnalysisFailed(*result.Failed)
	default:
		return fmt.Errorf("analysis result outcome %q is unsupported", result.Outcome)
	}
}

func (result AnalysisResult) MarshalJSON() ([]byte, error) {
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return nil, errors.New("analysis result must contain exactly one outcome value")
	}
	switch result.Outcome {
	case AnalysisOutcomeSucceeded:
		if result.Succeeded == nil {
			return nil, errors.New("succeeded analysis result requires success data")
		}
		return json.Marshal(struct {
			FormatVersion int             `json:"formatVersion"`
			Outcome       AnalysisOutcome `json:"outcome"`
			Files         []AnalysisFile  `json:"files"`
		}{
			FormatVersion: result.FormatVersion,
			Outcome:       result.Outcome,
			Files:         result.Succeeded.Files,
		})
	case AnalysisOutcomeFailed:
		if result.Failed == nil {
			return nil, errors.New("failed analysis result requires failure data")
		}
		return json.Marshal(struct {
			FormatVersion int             `json:"formatVersion"`
			Outcome       AnalysisOutcome `json:"outcome"`
			Error         AnalysisError   `json:"error"`
		}{
			FormatVersion: result.FormatVersion,
			Outcome:       result.Outcome,
			Error:         result.Failed.Error,
		})
	default:
		return nil, fmt.Errorf("analysis result outcome %q is unsupported", result.Outcome)
	}
}

func (result *AnalysisResult) UnmarshalJSON(raw []byte) error {
	var header struct {
		FormatVersion int             `json:"formatVersion"`
		Outcome       AnalysisOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}
	*result = AnalysisResult{
		FormatVersion: header.FormatVersion,
		Outcome:       header.Outcome,
	}
	switch header.Outcome {
	case AnalysisOutcomeSucceeded:
		var wire struct {
			FormatVersion int             `json:"formatVersion"`
			Outcome       AnalysisOutcome `json:"outcome"`
			Files         []AnalysisFile  `json:"files"`
		}
		if err := decodeClosedAnalysisResult(raw, &wire); err != nil {
			return err
		}
		result.Succeeded = &AnalysisSucceeded{Files: wire.Files}
	case AnalysisOutcomeFailed:
		var wire struct {
			FormatVersion int             `json:"formatVersion"`
			Outcome       AnalysisOutcome `json:"outcome"`
			Error         AnalysisError   `json:"error"`
		}
		if err := decodeClosedAnalysisResult(raw, &wire); err != nil {
			return err
		}
		result.Failed = &AnalysisFailed{Error: wire.Error}
	default:
		return fmt.Errorf("analysis result outcome %q is unsupported", header.Outcome)
	}
	return nil
}

func decodeClosedAnalysisResult(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return ensureEOF(decoder, "analysis result")
}

func validateAnalysisSucceeded(succeeded AnalysisSucceeded) error {
	if succeeded.Files == nil {
		return errors.New("analysis result files must be an array")
	}
	if len(succeeded.Files) != 1 && len(succeeded.Files) != 3 {
		return errors.New("analysis result files must contain exactly one or three entries")
	}
	if succeeded.Files[0].Path != AnalysisBuildPlanPath {
		return fmt.Errorf(
			"analysis result files[0].path = %q, want %q",
			succeeded.Files[0].Path,
			AnalysisBuildPlanPath,
		)
	}
	plan, err := ParseBuildPlan([]byte(succeeded.Files[0].Content))
	if err != nil {
		return fmt.Errorf("analysis result build plan: %w", err)
	}
	declarations := buildPlanProgramDeclarations(plan)
	if len(declarations) == 0 {
		if len(succeeded.Files) != 1 {
			return errors.New(
				"workspace-only analysis result must contain only the build plan",
			)
		}
		return nil
	}
	if len(succeeded.Files) != 3 {
		return errors.New(
			"program-backed analysis result must contain all generated program files",
		)
	}
	if succeeded.Files[1].Path != AnalysisDeclarationsPath {
		return fmt.Errorf(
			"analysis result files[1].path = %q, want %q",
			succeeded.Files[1].Path,
			AnalysisDeclarationsPath,
		)
	}
	if succeeded.Files[2].Path != AnalysisProgramEntryPath {
		return fmt.Errorf(
			"analysis result files[2].path = %q, want %q",
			succeeded.Files[2].Path,
			AnalysisProgramEntryPath,
		)
	}
	locator, err := ParseDeclarationLocator([]byte(succeeded.Files[1].Content))
	if err != nil {
		return fmt.Errorf("analysis result declaration locator: %w", err)
	}
	if len(locator.Declarations) != len(declarations) {
		return errors.New(
			"analysis result declaration locator does not match build plan",
		)
	}
	for index, declaration := range declarations {
		located := locator.Declarations[index]
		if located.Kind != declaration.Kind ||
			located.DeclaredID != declaration.DeclaredID {
			return fmt.Errorf(
				"analysis result declaration locator does not match build plan at position %d",
				index,
			)
		}
	}
	if succeeded.Files[2].Content != ProgramEntry {
		return errors.New("analysis result program entry does not match fixed v0 bytes")
	}
	return nil
}

func validateAnalysisFailed(failed AnalysisFailed) error {
	if failed.Error.Reason != AnalysisFailureReason {
		return fmt.Errorf(
			"analysis failure reason %q is unsupported",
			failed.Error.Reason,
		)
	}
	if !utf8.ValidString(failed.Error.Message) ||
		len(failed.Error.Message) > maxBuildFailureMessageBytes ||
		strings.TrimSpace(failed.Error.Message) == "" {
		return fmt.Errorf(
			"analysis failure message must be nonblank UTF-8 of at most %d bytes",
			maxBuildFailureMessageBytes,
		)
	}
	return nil
}

func cloneAnalysisResult(result AnalysisResult) AnalysisResult {
	if result.Succeeded != nil {
		files := append([]AnalysisFile(nil), result.Succeeded.Files...)
		result.Succeeded = &AnalysisSucceeded{Files: files}
	}
	if result.Failed != nil {
		failed := *result.Failed
		result.Failed = &failed
	}
	return result
}
