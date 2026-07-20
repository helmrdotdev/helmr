//go:build !linux

package guestd

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/deployment"
)

func dependencyResolveProfile() (bool, error) {
	return false, errors.New("dependency guest profile requires Linux")
}

func stageDependencyComponents(
	context.Context,
	deployment.ManagerRequest,
) (stagedDependencyComponents, error) {
	return nil, errors.New("dependency component staging requires Linux")
}

func activateDependencyManager(
	context.Context,
	deployment.ManagerRequest,
	stagedDependencyComponents,
) (deployment.ManagerMetadata, error) {
	return deployment.ManagerMetadata{}, errors.New(
		"dependency manager activation requires Linux",
	)
}
