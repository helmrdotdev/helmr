//go:build !linux

package deployment

import "errors"

func runVerifierChild(verifierJob) error {
	return errors.New("artifact verifier requires Linux")
}
