package deployment

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

const (
	BuildResultFormatVersion = 0

	BuildOutcomeSucceeded = BuildOutcome("succeeded")
	BuildOutcomeFailed    = BuildOutcome("failed")

	BuildFailureInvalidSource        = BuildFailureReason("invalid_source")
	BuildFailureManagerNotFound      = BuildFailureReason("manager_not_found")
	BuildFailureManagerUnsupported   = BuildFailureReason("unsupported_manager_protocol")
	BuildFailureLockfileUnsupported  = BuildFailureReason("unsupported_lockfile_format")
	BuildFailureUnsupportedToolchain = BuildFailureReason("unsupported_toolchain")
	BuildFailureManagerFailed        = BuildFailureReason("manager_failed")
	BuildFailureNetworkDenied        = BuildFailureReason("build_network_denied")
	BuildFailureNetworkLimit         = BuildFailureReason("build_network_limit")
	BuildFailureTransformFailed      = BuildFailureReason("transform_failed")
	BuildFailureVerificationFailed   = BuildFailureReason("verification_failed")
	BuildFailureInvalidPlan          = BuildFailureReason("invalid_plan")
	BuildFailureWorkspaceImageFailed = BuildFailureReason("workspace_image_failed")
	BuildFailureOutputInvalid        = BuildFailureReason("output_invalid")

	WorkspaceImageArtifactMediaType = "application/vnd.helmr.workspace-image.v0.oci-tar"

	maxBuildResultBytes               = 40 << 20
	maxBuildFailureMessageBytes       = 16 << 10
	maxBuildLogBytes                  = 16 << 20
	maxWorkspaceImageBytes      int64 = 17179869184
)

type BuildOutcome string
type BuildFailureReason string

type BuildResult struct {
	FormatVersion int          `json:"-"`
	Outcome       BuildOutcome `json:"-"`
	Succeeded     *BuildSucceeded
	Failed        *BuildFailed
	Logs          *BuildLogs
}

type BuildSucceeded struct {
	Plan            BuildPlan        `json:"plan"`
	Provenance      BuildProvenance  `json:"provenance"`
	Program         *ProgramOutput   `json:"program,omitempty"`
	WorkspaceImages []WorkspaceImage `json:"workspaceImages"`
}

type BuildFailed struct {
	Error BuildError `json:"error"`
}

type BuildError struct {
	ReasonCode BuildFailureReason `json:"reasonCode"`
	Message    string             `json:"message"`
}

type BuildLogs struct {
	ExitStatus   int32  `json:"exitStatus"`
	StderrBase64 string `json:"stderrBase64"`
	StdoutBase64 string `json:"stdoutBase64"`
	Truncated    bool   `json:"truncated"`
}

type WorkspaceImage struct {
	DeclaredID string                 `json:"declaredId"`
	Artifact   WorkspaceImageArtifact `json:"artifact"`
}

type WorkspaceImageArtifact struct {
	Digest       string              `json:"digest"`
	SizeBytes    int64               `json:"sizeBytes"`
	MediaType    string              `json:"mediaType"`
	Architecture RuntimeArchitecture `json:"architecture"`
}

func ParseBuildResult(raw []byte) (BuildResult, error) {
	if len(raw) == 0 || len(raw) > maxBuildResultBytes {
		return BuildResult{}, fmt.Errorf("build result size is outside [1,%d]", maxBuildResultBytes)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return BuildResult{}, fmt.Errorf("canonicalize build result: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return BuildResult{}, errors.New("build result is not RFC 8785 canonical JSON")
	}

	var result BuildResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return BuildResult{}, fmt.Errorf("decode build result: %w", err)
	}
	if err := ensureEOF(decoder, "build result"); err != nil {
		return BuildResult{}, err
	}
	if err := ValidateBuildResultContract(result); err != nil {
		return BuildResult{}, err
	}
	complete, err := CanonicalBuildResult(result)
	if err != nil {
		return BuildResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return BuildResult{}, errors.New("build result does not match the complete canonical v0 shape")
	}
	return result, nil
}

func CanonicalBuildResult(result BuildResult) ([]byte, error) {
	if err := ValidateBuildResultContract(result); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("encode build result: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize build result: %w", err)
	}
	if len(canonical) > maxBuildResultBytes {
		return nil, fmt.Errorf("build result size is outside [1,%d]", maxBuildResultBytes)
	}
	return canonical, nil
}

func ValidateBuildResultContract(result BuildResult) error {
	if result.FormatVersion != BuildResultFormatVersion {
		return fmt.Errorf(
			"build result formatVersion = %d, want %d",
			result.FormatVersion,
			BuildResultFormatVersion,
		)
	}
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return errors.New("build result must contain exactly one outcome value")
	}
	if result.Logs != nil {
		if err := validateBuildLogs(*result.Logs); err != nil {
			return err
		}
	}
	switch result.Outcome {
	case BuildOutcomeSucceeded:
		if result.Succeeded == nil {
			return errors.New("succeeded build result requires success data")
		}
		return validateBuildSucceeded(*result.Succeeded)
	case BuildOutcomeFailed:
		if result.Failed == nil {
			return errors.New("failed build result requires failure data")
		}
		return validateBuildFailed(*result.Failed)
	default:
		return fmt.Errorf("build result outcome %q is unsupported", result.Outcome)
	}
}

func ValidateBuildResultTarget(
	result BuildResult,
	runtimeDigest string,
	architecture RuntimeArchitecture,
) error {
	if err := ValidateBuildResultContract(result); err != nil {
		return err
	}
	if !sha256DigestPattern.MatchString(runtimeDigest) {
		return errors.New("build result target runtime digest is not a lowercase SHA-256 digest")
	}
	if !validArchitecture(architecture) {
		return fmt.Errorf("build result target architecture %q is unsupported", architecture)
	}
	if result.Outcome == BuildOutcomeFailed {
		return nil
	}
	succeeded := result.Succeeded
	if succeeded.Provenance.RuntimeDigest != runtimeDigest {
		return errors.New("build result provenance runtime digest does not match target")
	}
	if succeeded.Provenance.Architecture != architecture {
		return errors.New("build result provenance architecture does not match target")
	}
	if succeeded.Program != nil {
		if succeeded.Program.Index.RuntimeDigest != runtimeDigest {
			return errors.New("build result program runtime digest does not match target")
		}
		if succeeded.Program.Index.Architecture != architecture {
			return errors.New("build result program architecture does not match target")
		}
	}
	for _, workspace := range buildPlanWorkspaces(succeeded.Plan) {
		if workspace.Architecture != architecture {
			return fmt.Errorf(
				"build result workspace %q architecture does not match target",
				workspace.DeclaredID,
			)
		}
	}
	return nil
}

func (result BuildResult) MarshalJSON() ([]byte, error) {
	if (result.Succeeded == nil) == (result.Failed == nil) {
		return nil, errors.New("build result must contain exactly one outcome value")
	}
	switch result.Outcome {
	case BuildOutcomeSucceeded:
		if result.Succeeded == nil {
			return nil, errors.New("succeeded build result requires success data")
		}
		return json.Marshal(struct {
			FormatVersion   int              `json:"formatVersion"`
			Outcome         BuildOutcome     `json:"outcome"`
			Plan            BuildPlan        `json:"plan"`
			Provenance      BuildProvenance  `json:"provenance"`
			Program         *ProgramOutput   `json:"program,omitempty"`
			Logs            *BuildLogs       `json:"logs,omitempty"`
			WorkspaceImages []WorkspaceImage `json:"workspaceImages"`
		}{
			result.FormatVersion,
			result.Outcome,
			result.Succeeded.Plan,
			result.Succeeded.Provenance,
			result.Succeeded.Program,
			result.Logs,
			result.Succeeded.WorkspaceImages,
		})
	case BuildOutcomeFailed:
		if result.Failed == nil {
			return nil, errors.New("failed build result requires failure data")
		}
		return json.Marshal(struct {
			FormatVersion int          `json:"formatVersion"`
			Outcome       BuildOutcome `json:"outcome"`
			Error         BuildError   `json:"error"`
			Logs          *BuildLogs   `json:"logs,omitempty"`
		}{
			result.FormatVersion,
			result.Outcome,
			result.Failed.Error,
			result.Logs,
		})
	default:
		return nil, fmt.Errorf("build result outcome %q is unsupported", result.Outcome)
	}
}

func (result *BuildResult) UnmarshalJSON(raw []byte) error {
	var header struct {
		FormatVersion int          `json:"formatVersion"`
		Outcome       BuildOutcome `json:"outcome"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return err
	}

	*result = BuildResult{
		FormatVersion: header.FormatVersion,
		Outcome:       header.Outcome,
	}
	switch header.Outcome {
	case BuildOutcomeSucceeded:
		var wire struct {
			FormatVersion   int              `json:"formatVersion"`
			Outcome         BuildOutcome     `json:"outcome"`
			Plan            BuildPlan        `json:"plan"`
			Provenance      BuildProvenance  `json:"provenance"`
			Program         *ProgramOutput   `json:"program,omitempty"`
			Logs            *BuildLogs       `json:"logs,omitempty"`
			WorkspaceImages []WorkspaceImage `json:"workspaceImages"`
		}
		if err := decodeClosedBuildResult(raw, &wire); err != nil {
			return err
		}
		result.Succeeded = &BuildSucceeded{
			Plan:            wire.Plan,
			Provenance:      wire.Provenance,
			Program:         wire.Program,
			WorkspaceImages: wire.WorkspaceImages,
		}
		result.Logs = wire.Logs
	case BuildOutcomeFailed:
		var wire struct {
			FormatVersion int          `json:"formatVersion"`
			Outcome       BuildOutcome `json:"outcome"`
			Error         BuildError   `json:"error"`
			Logs          *BuildLogs   `json:"logs,omitempty"`
		}
		if err := decodeClosedBuildResult(raw, &wire); err != nil {
			return err
		}
		result.Failed = &BuildFailed{Error: wire.Error}
		result.Logs = wire.Logs
	default:
		return fmt.Errorf("build result outcome %q is unsupported", header.Outcome)
	}
	return nil
}

func decodeClosedBuildResult(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	return ensureEOF(decoder, "build result")
}

func validateBuildSucceeded(succeeded BuildSucceeded) error {
	if _, err := CanonicalBuildPlan(succeeded.Plan); err != nil {
		return fmt.Errorf("build result plan: %w", err)
	}
	if err := validateBuildProvenance("build result provenance", succeeded.Provenance); err != nil {
		return err
	}
	if succeeded.WorkspaceImages == nil {
		return errors.New("build result workspaceImages must be an array")
	}

	programDeclarations := buildPlanProgramDeclarations(succeeded.Plan)
	if len(programDeclarations) == 0 {
		if succeeded.Program != nil {
			return errors.New("workspace-only build result must not contain program")
		}
	} else {
		if succeeded.Program == nil {
			return errors.New("program-backed build result requires program")
		}
		if err := ValidateProgramOutput(*succeeded.Program); err != nil {
			return fmt.Errorf("build result program: %w", err)
		}
		if !equalProgramDeclarations(
			succeeded.Program.Index.Declarations,
			programDeclarations,
		) {
			return errors.New("build result program declarations do not match plan")
		}
		if succeeded.Program.Index.Architecture != succeeded.Provenance.Architecture ||
			succeeded.Program.Index.BuildContractVersion != succeeded.Provenance.BuildContractVersion ||
			succeeded.Program.Index.Manager != succeeded.Provenance.Manager ||
			succeeded.Program.Index.RuntimeDigest != succeeded.Provenance.RuntimeDigest ||
			succeeded.Program.Index.StandardToolchainDigest != succeeded.Provenance.StandardToolchainDigest ||
			succeeded.Program.Index.Submitted != succeeded.Provenance.Submitted {
			return errors.New("build result program provenance does not match")
		}
	}

	workspaces := buildPlanWorkspaces(succeeded.Plan)
	for _, workspace := range workspaces {
		if workspace.Architecture != succeeded.Provenance.Architecture {
			return fmt.Errorf(
				"build result workspace %q architecture does not match provenance",
				workspace.DeclaredID,
			)
		}
	}
	if len(succeeded.WorkspaceImages) != len(workspaces) {
		return errors.New("build result workspaceImages do not match plan")
	}
	for index, image := range succeeded.WorkspaceImages {
		workspace := workspaces[index]
		if image.DeclaredID != workspace.DeclaredID {
			return fmt.Errorf("build result workspaceImages[%d] declaredId does not match plan", index)
		}
		if !sha256DigestPattern.MatchString(image.Artifact.Digest) {
			return fmt.Errorf("build result workspaceImages[%d] digest is not a lowercase SHA-256 digest", index)
		}
		if image.Artifact.SizeBytes < 1 || image.Artifact.SizeBytes > maxWorkspaceImageBytes {
			return fmt.Errorf(
				"build result workspaceImages[%d] sizeBytes is outside [1,%d]",
				index,
				maxWorkspaceImageBytes,
			)
		}
		if image.Artifact.MediaType != WorkspaceImageArtifactMediaType {
			return fmt.Errorf(
				"build result workspaceImages[%d] mediaType = %q, want %q",
				index,
				image.Artifact.MediaType,
				WorkspaceImageArtifactMediaType,
			)
		}
		if image.Artifact.Architecture != workspace.Architecture {
			return fmt.Errorf("build result workspaceImages[%d] architecture does not match plan", index)
		}
	}
	return nil
}

func validateBuildFailed(failed BuildFailed) error {
	switch failed.Error.ReasonCode {
	case BuildFailureInvalidSource,
		BuildFailureManagerNotFound,
		BuildFailureManagerUnsupported,
		BuildFailureLockfileUnsupported,
		BuildFailureUnsupportedToolchain,
		BuildFailureManagerFailed,
		BuildFailureNetworkDenied,
		BuildFailureNetworkLimit,
		BuildFailureTransformFailed,
		BuildFailureVerificationFailed,
		BuildFailureInvalidPlan,
		BuildFailureWorkspaceImageFailed,
		BuildFailureOutputInvalid:
	default:
		return fmt.Errorf("build failure reasonCode %q is unsupported", failed.Error.ReasonCode)
	}
	if !utf8.ValidString(failed.Error.Message) ||
		len(failed.Error.Message) > maxBuildFailureMessageBytes ||
		strings.TrimSpace(failed.Error.Message) == "" {
		return fmt.Errorf(
			"build failure message must be nonblank UTF-8 of at most %d bytes",
			maxBuildFailureMessageBytes,
		)
	}
	return nil
}

func validateBuildLogs(logs BuildLogs) error {
	stdout, err := base64.StdEncoding.DecodeString(logs.StdoutBase64)
	if err != nil {
		return errors.New("build logs stdoutBase64 is invalid")
	}
	stderr, err := base64.StdEncoding.DecodeString(logs.StderrBase64)
	if err != nil {
		return errors.New("build logs stderrBase64 is invalid")
	}
	if len(stdout)+len(stderr) > maxBuildLogBytes {
		return fmt.Errorf("build logs exceed %d decoded bytes", maxBuildLogBytes)
	}
	if logs.ExitStatus < -1 || logs.ExitStatus > 255 {
		return errors.New("build logs exitStatus is outside [-1,255]")
	}
	return nil
}

type buildPlanWorkspace struct {
	DeclaredID   string
	Architecture RuntimeArchitecture
}

func buildPlanWorkspaces(plan BuildPlan) []buildPlanWorkspace {
	workspaces := make([]buildPlanWorkspace, 0)
	for _, definition := range plan.Definitions {
		if definition.Workspace == nil {
			continue
		}
		workspaces = append(workspaces, buildPlanWorkspace{
			DeclaredID:   definition.DeclaredID,
			Architecture: definition.Workspace.Architecture,
		})
	}
	return workspaces
}

func buildPlanProgramDeclarations(plan BuildPlan) []ProgramDeclaration {
	declarations := make([]ProgramDeclaration, 0)
	for _, definition := range plan.Definitions {
		switch definition.Kind {
		case DefinitionKindTask:
			slots := []DeclarationSlot{DeclarationSlotHandler}
			if definition.Task.Payload.Kind == SchemaKindStandard {
				slots = append(slots, DeclarationSlotPayloadSchema)
			}
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindTask,
				DeclaredID: definition.DeclaredID,
				Slots:      slots,
			})
		case DefinitionKindActor:
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindActor,
				DeclaredID: definition.DeclaredID,
				Slots:      []DeclarationSlot{DeclarationSlotHandler},
			})
		case DefinitionKindRunStream:
			declarations = append(declarations, ProgramDeclaration{
				Kind:       DeclarationKindRunStream,
				DeclaredID: definition.DeclaredID,
				Slots:      []DeclarationSlot{DeclarationSlotSchema},
			})
		}
	}
	return declarations
}

func equalProgramDeclarations(left, right []ProgramDeclaration) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Kind != right[index].Kind ||
			left[index].DeclaredID != right[index].DeclaredID ||
			!equalDeclarationSlots(left[index].Slots, right[index].Slots) {
			return false
		}
	}
	return true
}

func equalDeclarationSlots(left, right []DeclarationSlot) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
