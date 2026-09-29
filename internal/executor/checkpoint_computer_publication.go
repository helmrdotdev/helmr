package executor

import (
	"context"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type CheckpointComputerPublicationClient interface {
	RegisterCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error
	CertifyCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error
	ReuseCheckpointComputerObject(context.Context, workerapi.CheckpointComputerObjectRequest) error
}
type checkpointComputerPublisher struct {
	client  CheckpointComputerPublicationClient
	objects cas.ImmutableStore
	request workerapi.CheckpointComputerObjectRequest
}

func (p checkpointComputerPublisher) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.RegisterCheckpointComputerObject(ctx, r) })
}
func (p checkpointComputerPublisher) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.CertifyCheckpointComputerObject(ctx, r) })
}
func (p checkpointComputerPublisher) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.ReuseCheckpointComputerObject(ctx, r) })
}
func (p checkpointComputerPublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (o cas.Object, err error) {
	err = retryCheckpointUpload(ctx, func() error { o, err = p.objects.Publish(ctx, d, f); return err })
	return
}

var _ disk.ContinuationPublication = checkpointComputerPublisher{}
