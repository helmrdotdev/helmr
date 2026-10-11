package artifact

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
	productversion "github.com/helmrdotdev/helmr/internal/version"
)

type ProgramCompilerResult struct {
	APIVersion    string            `json:"apiVersion"`
	Bundler       BundlerIdentity   `json:"bundler"`
	NodeVersion   string            `json:"nodeVersion"`
	Config        ProgramPathDigest `json:"config"`
	PayloadDigest string            `json:"payloadDigest"`
	Modules       []string          `json:"modules"`
}

func ParseProgramCompilerResult(raw []byte) (ProgramCompilerResult, error) {
	if len(raw) == 0 || len(raw) > int(MaxProgramFileSizeBytes) {
		return ProgramCompilerResult{}, fmt.Errorf(
			"program compiler result size is outside [1,%d]",
			MaxProgramFileSizeBytes,
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
	if err := jsoncanon.RequireEOF(decoder, "program compiler result"); err != nil {
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
			"program compiler result does not match the complete canonical v0 shape",
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
	if result.APIVersion != "helmr.compiler.v0" || result.NodeVersion != productversion.Node() {
		return errors.New("program compiler execution contract is invalid")
	}
	if err := ValidateBundlerIdentity(result.Bundler); err != nil {
		return err
	}
	if result.Config.Path != "helmr/config.json" || !sha256DigestPattern.MatchString(result.Config.Digest) || !sha256DigestPattern.MatchString(result.PayloadDigest) {
		return errors.New("program compiler input authority is invalid")
	}
	if result.Modules == nil {
		return errors.New("program compiler collections must be arrays")
	}
	for i, value := range result.Modules {
		if err := validateDeclarationModulePath(value); err != nil {
			return err
		}
		if i > 0 && result.Modules[i-1] >= value {
			return errors.New("discovery candidates are not in canonical order")
		}
	}
	return nil
}
func ValidateProgramCompilerAuthority(result ProgramCompilerResult, compiler CompilerInputs, nodeVersion string) error {
	if err := ValidateCompilerInputs(compiler); err != nil {
		return err
	}
	if result.APIVersion != compiler.APIVersion || result.Bundler != compiler.Bundler || result.NodeVersion != nodeVersion {
		return errors.New("program compiler result does not match compiler/runtime authority")
	}
	return nil
}
func VerifyProgramCompilerFiles(ctx context.Context, artifact *Tree, result ProgramCompilerResult) error {
	if err := VerifyProgramManifestFiles(ctx, artifact, ProgramManifestFromCompilerResult(result, "sha256:"+strings.Repeat("0", 64))); err != nil {
		return err
	}
	for _, candidate := range result.Modules {
		if err := VerifyDeclarationModule(artifact, candidate); err != nil {
			return err
		}
	}
	return nil
}
func ValidateProgramCompilerLocators(result ProgramCompilerResult, index DefinitionIndex) error {
	if err := ValidateDefinitionIndex(index); err != nil {
		return err
	}
	for _, a := range index.Agents {
		if _, found := slices.BinarySearch(result.Modules, a.ModulePath); !found {
			return errors.New("agent module is not a discovery candidate")
		}
	}
	for _, c := range index.Computers {
		if _, found := slices.BinarySearch(result.Modules, c.ModulePath); !found {
			return errors.New("computer module is not a discovery candidate")
		}
	}
	return nil
}

func VerifyProgramPathDigest(
	ctx context.Context,
	artifact *Tree,
	file ProgramPathDigest,
) error {
	raw, err := artifact.Read(ctx, file.Path, MaxProgramFileSizeBytes)
	if err != nil {
		return fmt.Errorf("program file %q: %w", file.Path, err)
	}
	digest := sha256.Sum256(raw)
	if "sha256:"+hex.EncodeToString(digest[:]) != file.Digest {
		return fmt.Errorf("program file %q digest does not match authority", file.Path)
	}
	return nil
}
