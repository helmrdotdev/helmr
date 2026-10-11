//go:build !linux

package builder

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/bundle"
)

type ProgramInput struct {
	ProjectDirectory string
	WorkDirectory    string
	NodePath         string
	ConfigPath       string
	BundlePath       string
	ProgramCompiler  string
	Compiler         artifact.CompilerInputs
	Runtime          artifact.RuntimeDescriptor
	RuntimeMetadata  artifact.RuntimeMetadata
}

type PreparedProgramInput struct {
	PreparedDirectory string
	WorkDirectory     string
	ProgramObjectPath string
	SquashFSEncoder   string
	Compiler          artifact.CompilerInputs
	Runtime           artifact.RuntimeDescriptor
	RuntimeMetadata   artifact.RuntimeMetadata
	ComputerSeeds     []bundle.ComputerSeed
}

type ProgramResult struct {
	Program      artifact.ProgramOutput
	Config       artifact.BuildConfig
	Verification VerificationResult
	ObjectPath   string
}

func PrepareProgram(context.Context, ProgramInput, string) error {
	return errors.New("canonical Program preparation requires linux/amd64 BuildKit")
}

func BuildPreparedProgram(context.Context, PreparedProgramInput) (ProgramResult, error) {
	return ProgramResult{}, errors.New("canonical prepared Program builds require linux/amd64 BuildKit")
}
