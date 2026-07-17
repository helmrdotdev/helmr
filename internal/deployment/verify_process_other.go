//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func runProgramVerifierProcess(
	context.Context,
	programVerifierProcessConfig,
) (programVerifierProcessResult, error) {
	return programVerifierProcessResult{}, errors.New("program verifier requires Linux cgroup v2")
}
