package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

var exactReleaseVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type CompilerEntrypoint struct {
	APIVersion string `json:"apiVersion"`
	Digest     string `json:"digest"`
	Entrypoint string `json:"entrypoint"`
}

type EsbuildInputs struct {
	APIPackageDigest string `json:"apiPackageDigest"`
	BinaryDigest     string `json:"binaryDigest"`
	BinaryPath       string `json:"binaryPath"`
	PackagePath      string `json:"packagePath"`
	Version          string `json:"version"`
}

type CompilerOutputContract struct {
	Aggregate    string `json:"aggregate"`
	FinalModules string `json:"finalModules"`
	SharedChunks bool   `json:"sharedChunks"`
	SourceMaps   string `json:"sourceMaps"`
}

type CompilerSourceContract struct {
	DeclarationExtensions []string `json:"declarationExtensions"`
	PackageDependencies   string   `json:"packageDependencies"`
	Semantics             string   `json:"semantics"`
	WorkspaceDependencies string   `json:"workspaceDependencies"`
}

// CompilerInputs describes the Product-owned compiler inside the canonical
// builder. It does not describe dependency installation or a package manager.
type CompilerInputs struct {
	APIVersion            string                 `json:"apiVersion"`
	ConfigEvaluator       CompilerEntrypoint     `json:"configEvaluator"`
	Esbuild               EsbuildInputs          `json:"esbuild"`
	OptionsContractDigest string                 `json:"optionsContractDigest"`
	Output                CompilerOutputContract `json:"output"`
	ProgramCompiler       CompilerEntrypoint     `json:"programCompiler"`
	Source                CompilerSourceContract `json:"source"`
}

func ParseCompilerInputs(raw []byte) (CompilerInputs, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return CompilerInputs{}, errors.New("compiler inputs size is outside [1,65536]")
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return CompilerInputs{}, fmt.Errorf("canonicalize compiler inputs: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return CompilerInputs{}, errors.New("compiler inputs are not RFC 8785 canonical JSON")
	}
	var inputs CompilerInputs
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inputs); err != nil {
		return CompilerInputs{}, fmt.Errorf("decode compiler inputs: %w", err)
	}
	if err := ensureEOF(decoder, "compiler inputs"); err != nil {
		return CompilerInputs{}, err
	}
	if err := ValidateCompilerInputs(inputs); err != nil {
		return CompilerInputs{}, err
	}
	complete, err := CanonicalCompilerInputs(inputs)
	if err != nil {
		return CompilerInputs{}, err
	}
	if !bytes.Equal(raw, complete) {
		return CompilerInputs{}, errors.New("compiler inputs do not match the complete canonical v0 shape")
	}
	return inputs, nil
}

func CanonicalCompilerInputs(inputs CompilerInputs) ([]byte, error) {
	if err := ValidateCompilerInputs(inputs); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		return nil, fmt.Errorf("encode compiler inputs: %w", err)
	}
	canonical, err := jsoncanon.Transform(raw)
	if err != nil {
		return nil, fmt.Errorf("canonicalize compiler inputs: %w", err)
	}
	if len(canonical) == 0 || len(canonical) > 64<<10 {
		return nil, errors.New("compiler inputs size is outside [1,65536]")
	}
	return canonical, nil
}

func ValidateCompilerInputs(input CompilerInputs) error {
	if input.APIVersion != "helmr.compiler.v0" ||
		input.ConfigEvaluator.APIVersion != ConfigEvaluatorContract ||
		input.ConfigEvaluator.Entrypoint != "/nix/helmr/config-evaluator.mjs" ||
		input.ProgramCompiler.APIVersion != "helmr.compiler.v0" ||
		input.ProgramCompiler.Entrypoint != "/nix/helmr/program-compiler.mjs" ||
		input.Esbuild.Version != "0.28.1" ||
		input.Esbuild.BinaryPath != "/nix/helmr/esbuild" ||
		input.Esbuild.PackagePath != "/nix/node_modules/esbuild" ||
		input.Output.Aggregate != "analysis-only" ||
		input.Output.FinalModules != "independent" ||
		input.Output.SharedChunks ||
		input.Output.SourceMaps != "external" ||
		input.Source.PackageDependencies != "external" ||
		input.Source.Semantics != "pinned-esbuild" ||
		input.Source.WorkspaceDependencies != "bundled" ||
		!slices.Equal(input.Source.DeclarationExtensions,
			[]string{".cjs", ".cts", ".js", ".jsx", ".mjs", ".mts", ".ts", ".tsx"}) {
		return errors.New("compiler inputs do not match the v0 contract")
	}
	for label, digest := range map[string]string{
		"Config Evaluator":          input.ConfigEvaluator.Digest,
		"esbuild API package":       input.Esbuild.APIPackageDigest,
		"esbuild binary":            input.Esbuild.BinaryDigest,
		"Compiler options contract": input.OptionsContractDigest,
		"program compiler":          input.ProgramCompiler.Digest,
	} {
		if !sha256DigestPattern.MatchString(digest) {
			return fmt.Errorf("%s digest is invalid", label)
		}
	}
	return nil
}

func parseReleaseVersion(value string) (int, int, int, bool) {
	if !exactReleaseVersionPattern.MatchString(value) {
		return 0, 0, 0, false
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(value, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

func compareReleaseVersion(major, minor, patch int, minimum string) int {
	var minimumMajor, minimumMinor, minimumPatch int
	_, _ = fmt.Sscanf(minimum, "%d.%d.%d", &minimumMajor, &minimumMinor, &minimumPatch)
	switch {
	case major != minimumMajor:
		return major - minimumMajor
	case minor != minimumMinor:
		return minor - minimumMinor
	default:
		return patch - minimumPatch
	}
}
