//go:build linux || darwin

package computerhost

import (
	"context"
	"io"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
)

type saveStoragePublisher struct{ *cas.File }

func (p saveStoragePublisher) Publish(ctx context.Context, d cas.Descriptor, f *os.File) (cas.Object, error) {
	return p.Put(ctx, d.MediaType, io.NewSectionReader(f, 0, d.SizeBytes))
}
