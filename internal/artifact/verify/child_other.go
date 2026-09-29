//go:build !linux

package verify

import "errors"

func runVerifierChild(verifierJob) error {
	return errors.New("artifact verifier requires Linux")
}
