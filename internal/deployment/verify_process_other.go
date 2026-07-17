//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func runProgramVerifierProcess(
	context.Context,
	programVerifierProcessConfig,
) ([]byte, error) {
	return nil, errors.New("program verifier requires Linux cgroup v2")
}
