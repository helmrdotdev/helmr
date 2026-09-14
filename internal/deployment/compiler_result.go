package deployment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type ProgramCompilerResult struct {
	APIVersion          string                     `json:"apiVersion"`
	Language            ModuleExecutionIdentity    `json:"language"`
	NodeVersion         string                     `json:"nodeVersion"`
	Config              ProgramPathDigest          `json:"config"`
	InputTreeDigest     string                     `json:"inputTreeDigest"`
	DiscoveryCandidates []string                   `json:"discoveryCandidates"`
	Selections          []ProgramCompilerSelection `json:"selections"`
}
type ProgramCompilerSelection struct {
	DeclaredID string          `json:"declaredId"`
	ExportName string          `json:"exportName"`
	Kind       DeclarationKind `json:"kind"`
	Slot       DeclarationSlot `json:"slot"`
	SourcePath string          `json:"sourcePath"`
}

func ParseProgramCompilerResult(raw []byte) (ProgramCompilerResult, error) {
	if len(raw) == 0 || len(raw) > int(maxProgramFileSizeBytes) {
		return ProgramCompilerResult{}, fmt.Errorf(
			"program compiler result size is outside [1,%d]",
			maxProgramFileSizeBytes,
		)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"canonicalize program compiler result: %w",
			err,
		)
	}
	if !bytes.Equal(raw, canonical) {
		return ProgramCompilerResult{}, errors.New(
			"program compiler result is not RFC 8785 canonical JSON",
		)
	}
	var result ProgramCompilerResult
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ProgramCompilerResult{}, fmt.Errorf(
			"decode program compiler result: %w",
			err,
		)
	}
	if err := ensureEOF(decoder, "program compiler result"); err != nil {
		return ProgramCompilerResult{}, err
	}
	if err := validateProgramCompilerResult(result); err != nil {
		return ProgramCompilerResult{}, err
	}
	complete, err := canonicalProgramCompilerResult(result)
	if err != nil {
		return ProgramCompilerResult{}, err
	}
	if !bytes.Equal(raw, complete) {
		return ProgramCompilerResult{}, errors.New(
			"program compiler result does not match the complete canonical v1 shape",
		)
	}
	return result, nil
}

func canonicalProgramCompilerResult(
	result ProgramCompilerResult,
) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return jsoncanon.Transform(raw)
}

func validateProgramCompilerResult(result ProgramCompilerResult) error {
	if result.APIVersion != "helmr.compiler.v1" || result.NodeVersion != "24.21.0" {
		return errors.New("program compiler execution contract is invalid")
	}
	if err := ValidateModuleExecutionIdentity(result.Language); err != nil {
		return err
	}
	if result.Config.Path != "helmr/config.json" || !sha256DigestPattern.MatchString(result.Config.Digest) || !sha256DigestPattern.MatchString(result.InputTreeDigest) {
		return errors.New("program compiler input authority is invalid")
	}
	if result.DiscoveryCandidates == nil || result.Selections == nil {
		return errors.New("program compiler collections must be arrays")
	}
	for i, value := range result.DiscoveryCandidates {
		if err := validateDeclarationSourcePath(value); err != nil {
			return err
		}
		if i > 0 && result.DiscoveryCandidates[i-1] >= value {
			return errors.New("discovery candidates are not in canonical order")
		}
	}
	for i, value := range result.Selections {
		if err := validateLocatedDeclaration(LocatedDeclaration{DeclaredID: value.DeclaredID, ExportName: value.ExportName, Kind: value.Kind, Slot: value.Slot, SourcePath: value.SourcePath}); err != nil {
			return err
		}
		if _, found := slices.BinarySearch(result.DiscoveryCandidates, value.SourcePath); !found {
			return errors.New("selection is not a discovery candidate")
		}
		if i > 0 && compareProgramCompilerSelection(result.Selections[i-1], value) >= 0 {
			return errors.New("selections are not in canonical order")
		}
	}
	return nil
}
func validateProgramCompilerAuthority(result ProgramCompilerResult, compiler CompilerInputs, nodeVersion string) error {
	if err := ValidateCompilerInputs(compiler); err != nil {
		return err
	}
	if result.APIVersion != compiler.APIVersion || result.Language != compiler.Language || result.NodeVersion != nodeVersion {
		return errors.New("program compiler result does not match compiler/runtime authority")
	}
	return nil
}
func verifyProgramCompilerFiles(ctx context.Context, artifact *inspectedArtifact, result ProgramCompilerResult) error {
	if err := verifyProgramManifestFiles(ctx, artifact, programManifestFromCompilerResult(result, "sha256:"+strings.Repeat("0", 64))); err != nil {
		return err
	}
	for _, candidate := range result.DiscoveryCandidates {
		if err := verifyDeclarationSource(artifact, candidate); err != nil {
			return err
		}
	}
	return nil
}
func validateProgramCompilerLocators(result ProgramCompilerResult, locator DeclarationLocator) error {
	if len(result.Selections) != len(locator.Declarations) {
		return errors.New("compiler selections do not match declaration locators")
	}
	for i, value := range result.Selections {
		d := locator.Declarations[i]
		if value.DeclaredID != d.DeclaredID || value.ExportName != d.ExportName || value.Kind != d.Kind || value.Slot != d.Slot || value.SourcePath != d.SourcePath {
			return errors.New("compiler selections do not match declaration locators")
		}
	}
	return nil
}
func compareProgramCompilerSelection(left, right ProgramCompilerSelection) int {
	leftKind := declarationKindOrder(left.Kind)
	rightKind := declarationKindOrder(right.Kind)
	if leftKind < rightKind {
		return -1
	}
	if leftKind > rightKind {
		return 1
	}
	return strings.Compare(
		left.DeclaredID+"\x00"+left.SourcePath+"\x00"+
			left.ExportName+"\x00"+string(left.Slot),
		right.DeclaredID+"\x00"+right.SourcePath+"\x00"+
			right.ExportName+"\x00"+string(right.Slot),
	)
}

func verifyProgramPathDigest(
	ctx context.Context,
	artifact *inspectedArtifact,
	file ProgramPathDigest,
) error {
	raw, err := artifact.read(ctx, file.Path, maxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("program file %q: %w", file.Path, err)
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != file.Digest {
		return fmt.Errorf("program file %q digest does not match authority", file.Path)
	}
	return nil
}
