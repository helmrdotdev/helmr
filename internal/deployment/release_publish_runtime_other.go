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
	return errors.New("platform Runtime publication requires Linux")
}
