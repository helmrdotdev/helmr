//go:build linux

package computer

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/cas"
	"io"
	"os"
)

// The test adapter uses the real file CAS and verifies the same read-only input
// contract as the production immutable publisher.
type seedTestPublisher struct{ cas.Store }

func (p seedTestPublisher) Publish(ctx context.Context, expected cas.Descriptor, file *os.File) (cas.Object, error) {
	if _, err := cas.InspectPublishedFile(file); err != nil {
		return cas.Object{}, err
	}
	if err := cas.VerifyDescriptorFile(ctx, expected, file); err != nil {
		return cas.Object{}, err
	}
	stage, err := p.Stage(ctx, expected.MediaType)
	if err != nil {
		return cas.Object{}, err
	}
	return cas.WriteStage(ctx, stage, io.NewSectionReader(file, 0, expected.SizeBytes))
}
