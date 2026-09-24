package executor

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"os"
)

type RunComputerPublicationClient interface {
	RegisterRunComputerObject(context.Context, workerapi.RunComputerObjectRequest) error
	CertifyRunComputerObject(context.Context, workerapi.RunComputerObjectRequest) error
	ReuseRunComputerObject(context.Context, workerapi.RunComputerObjectRequest) error
}
type runComputerPublisher struct {
	client  RunComputerPublicationClient
	objects cas.ImmutableStore
	request workerapi.RunComputerObjectRequest
}

func (p runComputerPublisher) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.RegisterRunComputerObject(ctx, r) })
}
func (p runComputerPublisher) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.CertifyRunComputerObject(ctx, r) })
}
func (p runComputerPublisher) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.ReuseRunComputerObject(ctx, r) })
}
func (p runComputerPublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (o cas.Object, err error) {
	err = retryCheckpointUpload(ctx, func() error { o, err = p.objects.Publish(ctx, d, f); return err })
	return
}

var _ computer.ContinuationPublication = runComputerPublisher{}
