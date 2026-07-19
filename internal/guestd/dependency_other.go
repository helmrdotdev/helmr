//go:build !linux

package guestd

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func stageDependencyComponents(
	context.Context,
	deployment.ManagerRequest,
) (stagedDependencyComponents, error) {
	return nil, errors.New("dependency component staging requires Linux")
}
