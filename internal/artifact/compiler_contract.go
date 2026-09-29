package artifact

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/jsoncanon"
)

type CompilerEntrypoint struct {
	APIVersion string `json:"apiVersion"`
	Digest     string `json:"digest"`
	Entrypoint string `json:"entrypoint"`
}

// BundlerIdentity identifies the build-only JavaScript generation contract.
type BundlerIdentity struct {
	APIVersion     string `json:"apiVersion"`
	EsbuildVersion string `json:"esbuildVersion"`
	APIDigest      string `json:"apiDigest"`
	BinaryDigest   string `json:"binaryDigest"`
}

func ValidateBundlerIdentity(value BundlerIdentity) error {
	if value.APIVersion != "helmr.bundle.v0" || value.EsbuildVersion != "0.28.2" || !sha256DigestPattern.MatchString(value.APIDigest) || !sha256DigestPattern.MatchString(value.BinaryDigest) {
		return errors.New("bundler identity does not match the v0 contract")
	}
	return nil
}

type CompilerInputs struct {
	APIVersion      string             `json:"apiVersion"`
	Bundler         BundlerIdentity    `json:"bundler"`
	ProgramCompiler CompilerEntrypoint `json:"programCompiler"`
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
	if err := jsoncanon.RequireEOF(decoder, "compiler inputs"); err != nil {
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
	if err := ValidateBundlerIdentity(input.Bundler); err != nil {
		return err
	}
	for _, entry := range []CompilerEntrypoint{input.ProgramCompiler} {
		if !sha256DigestPattern.MatchString(entry.Digest) {
			return errors.New("compiler entrypoint digest is invalid")
		}
	}
	return nil
}
