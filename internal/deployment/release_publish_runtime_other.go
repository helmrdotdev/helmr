//go:build !linux

package deployment

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/cas"
)

func publishVerifiedPlatformRuntime(
	context.Context,
	cas.ImmutableStore,
	string,
	RuntimeDescriptor,
) error {
	return errors.New("Platform Runtime publication requires Linux")
}
