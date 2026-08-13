//go:build !linux

package builder

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

type ProgramInput struct {
	ProjectDirectory  string
	WorkDirectory     string
	ProgramObjectPath string
	NodePath          string
	NodeLoader        string
	NodeLibraryPath   string
	ConfigEvaluator   string
	ProgramCompiler   string
	SquashFSEncoder   string
	Compiler          deployment.CompilerInputs
	Runtime           deployment.RuntimeDescriptor
	RuntimeMetadata   deployment.RuntimeMetadata
}

type ProgramResult struct {
	Program      deployment.ProgramOutput
	Config       deployment.BuildConfig
	Verification deployment.VerificationResult
	ObjectPath   string
}

func BuildProgram(context.Context, ProgramInput) (ProgramResult, error) {
	return ProgramResult{}, errors.New("canonical Program builds require linux/amd64 BuildKit")
}
