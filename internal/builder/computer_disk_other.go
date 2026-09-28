//go:build !linux

package builder

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func buildComputerDisk(context.Context, string, string, string, string, string) (deployment.BundleComputerImageArtifact, error) {
	return deployment.BundleComputerImageArtifact{}, errors.New("deployment disk generation requires the Linux builder")
}
