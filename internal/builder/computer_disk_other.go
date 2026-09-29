//go:build !linux

package builder

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/bundle"
)

func buildComputerDisk(context.Context, string, string, string, string, string) (bundle.ComputerImageArtifact, error) {
	return bundle.ComputerImageArtifact{}, errors.New("deployment disk generation requires the Linux builder")
}
