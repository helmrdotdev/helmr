//go:build !linux

package deployment

import (
	"context"
	"errors"
)

func buildManagerProject(
	context.Context,
	string,
	string,
	DependencySource,
) (*managerProject, error) {
	return nil, errors.New("manager projects require Linux")
}
