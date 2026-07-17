//go:build !linux

package deployment

import "errors"

func runProgramVerifierChild() error {
	return errors.New("program verifier requires Linux")
}
