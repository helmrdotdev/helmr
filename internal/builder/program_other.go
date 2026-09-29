//go:build !linux

package builder

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

type ProgramInput struct {
	ProjectDirectory string
	WorkDirectory    string
	NodePath         string
	ConfigPath       string
	BundlePath       string
	ProgramCompiler  string
	Compiler         deployment.CompilerInputs
	Runtime          deployment.RuntimeDescriptor
	RuntimeMetadata  deployment.RuntimeMetadata
}

type PreparedProgramInput struct {
	PreparedDirectory string
	WorkDirectory     string
	ProgramObjectPath string
	SquashFSEncoder   string
	Compiler          deployment.CompilerInputs
	Runtime           deployment.RuntimeDescriptor
	RuntimeMetadata   deployment.RuntimeMetadata
	ComputerImages    []deployment.BundleComputerImage
}

type ProgramResult struct {
	Program      deployment.ProgramOutput
	Config       deployment.BuildConfig
	Verification deployment.VerificationResult
	ObjectPath   string
}

func PrepareProgram(context.Context, ProgramInput, string) error {
	return errors.New("canonical Program preparation requires linux/amd64 BuildKit")
}

func BuildPreparedProgram(context.Context, PreparedProgramInput) (ProgramResult, error) {
	return ProgramResult{}, errors.New("canonical prepared Program builds require linux/amd64 BuildKit")
}
