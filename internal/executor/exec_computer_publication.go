package executor

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"os"
)

type execComputerPublisher struct {
	client  workerapi.WorkspaceMaterializerControlPlaneClient
	objects cas.ImmutableStore
	request workerapi.ExecComputerObjectRequest
}

func (p execComputerPublisher) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.RegisterExecComputerObject(ctx, r) })
}
func (p execComputerPublisher) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.CertifyExecComputerObject(ctx, r) })
}
func (p execComputerPublisher) Reuse(ctx context.Context, e blockformat.ObjectInspection) error {
	r := p.request
	r.Inspection = e
	return retryRunLeaseRequest(ctx, func(ctx context.Context) error { return p.client.ReuseExecComputerObject(ctx, r) })
}
func (p execComputerPublisher) Upload(ctx context.Context, d cas.Descriptor, f *os.File) (o cas.Object, err error) {
	err = retryCheckpointUpload(ctx, func() error { o, err = p.objects.Publish(ctx, d, f); return err })
	return
}

var _ computer.ContinuationPublication = execComputerPublisher{}
