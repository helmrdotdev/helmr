//go:build !linux

package worker

import "errors"

func PrepareProgramVerifierHost() error {
	return errors.New("program verifier requires Linux cgroup v2")
}

func programVerifierHostHealthy() bool {
	return false
}
