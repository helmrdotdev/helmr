//go:build !linux

package verify

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/cas"
)

// PublishPlatformRuntime publishes the operator-supplied Platform Runtime
// object at path after snapshotting it and verifying it in process against
// descriptor. Nothing reaches store unless verification succeeds.
func PublishPlatformRuntime(
	context.Context,
	cas.ImmutableStore,
	string,
	artifact.RuntimeDescriptor,
) error {
	return errors.New("platform Runtime publication requires Linux")
}
