package buildkit

import (
	"fmt"

	"github.com/helmrdotdev/helmr/internal/builder"
	bundlev0 "github.com/helmrdotdev/helmr/internal/proto/bundle/v0"
	"github.com/helmrdotdev/helmr/internal/secret"
)

func validateBuildSecrets(image *bundlev0.ImageSpec, subImages map[string]*bundlev0.ImageSpec, values map[string][]byte) error {
	required, err := builder.RequiredBuildSecrets(&bundlev0.Bundle{Image: image, SubImages: subImages})
	if err != nil {
		return err
	}
	for name := range required {
		if _, ok := values[name]; !ok {
			return fmt.Errorf("build secret %q is required by image build", name)
		}
	}
	return nil
}

func redactBuildError(err error, secrets map[string][]byte) string {
	if err == nil {
		return ""
	}
	patterns := make([][]byte, 0, len(secrets))
	for _, value := range secrets {
		patterns = append(patterns, value)
	}
	return secret.NewRedactor(patterns...).RedactString(err.Error())
}
