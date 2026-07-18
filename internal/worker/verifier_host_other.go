//go:build !linux

package worker

import "errors"

func PrepareVerifierHost() (string, error) {
	return "", errors.New("verifier requires Linux cgroup v2")
}

func programVerifierHostHealthy() bool {
	return false
}
