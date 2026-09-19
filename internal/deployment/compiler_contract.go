package deployment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/version"
)

type CompilerEntrypoint struct {
	APIVersion string `json:"apiVersion"`
	Digest     string `json:"digest"`
	Entrypoint string `json:"entrypoint"`
}

// ModuleExecutionIdentity binds the exact shared parser, emitter and source guard.
type ModuleExecutionIdentity struct {
	APIVersion        string `json:"apiVersion"`
	AdapterDigest     string `json:"adapterDigest"`
	TypeScriptDigest  string `json:"typescriptDigest"`
	TypeScriptVersion string `json:"typescriptVersion"`
}

func ValidateModuleExecutionIdentity(value ModuleExecutionIdentity) error {
	if value.APIVersion != "helmr.module-execution.v0" || value.TypeScriptVersion != version.RuntimeTypeScript() ||
		!sha256DigestPattern.MatchString(value.AdapterDigest) || !sha256DigestPattern.MatchString(value.TypeScriptDigest) {
		return errors.New("module execution identity does not match the v0 contract")
	}
	return nil
}

type CompilerInputs struct {
	APIVersion      string                  `json:"apiVersion"`
	Language        ModuleExecutionIdentity `json:"language"`
	ProgramCompiler CompilerEntrypoint      `json:"programCompiler"`
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
	if input.APIVersion != "helmr.compiler.v0" || input.ProgramCompiler.APIVersion != "helmr.compiler.v0" ||
		input.ProgramCompiler.Entrypoint != "/nix/helmr/program-compiler.mjs" {
		return errors.New("compiler inputs do not match the v0 contract")
	}
	if err := ValidateModuleExecutionIdentity(input.Language); err != nil {
		return err
	}
	for _, entry := range []CompilerEntrypoint{input.ProgramCompiler} {
		if !sha256DigestPattern.MatchString(entry.Digest) {
			return errors.New("compiler entrypoint digest is invalid")
		}
	}
	return nil
}
