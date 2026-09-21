//go:build !linux

package builder

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/deployment"
)

func buildWorkspaceDisk(context.Context, string, string, string, string, string) (deployment.BundleWorkspaceImageArtifact, error) {
	return deployment.BundleWorkspaceImageArtifact{}, errors.New("deployment disk generation requires the Linux builder")
}
