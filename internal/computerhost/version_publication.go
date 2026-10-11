package computerhost

import (
	"context"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type versionObjectPublisher interface {
	Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error)
}
