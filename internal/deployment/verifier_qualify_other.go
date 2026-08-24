//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func QualifyArtifactVerifier(context.Context, string, string) error {
	return errors.New("artifact verifier qualification requires Linux")
}
