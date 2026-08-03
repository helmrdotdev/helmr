//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func runVerifierProcess(
	context.Context,
	verifierProcessConfig,
) (verifierProcessResult, error) {
	return verifierProcessResult{}, errors.New("artifact verifier requires Linux cgroup v2")
}
