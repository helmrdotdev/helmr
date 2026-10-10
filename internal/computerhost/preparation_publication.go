package computerhost

import (
	"context"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type preparationPublisher struct {
	client   AllocatedPreparationClient
	executor workerapi.PreparationExecutor
	objects  versionObjectPublisher
}

func (p preparationPublisher) Register(ctx context.Context, inspection blockformat.ObjectInspection) error {
	return retryPreparation(ctx, func(ctx context.Context) error {
		return p.client.RegisterPreparationObject(ctx, workerapi.PreparationObject{Executor: p.executor, Inspection: inspection})
	})
}
func (p preparationPublisher) Certify(ctx context.Context, inspection blockformat.ObjectInspection) error {
	return retryPreparation(ctx, func(ctx context.Context) error {
		return p.client.CertifyPreparationObject(ctx, workerapi.PreparationObject{Executor: p.executor, Inspection: inspection})
	})
}
func (p preparationPublisher) Upload(ctx context.Context, descriptor cas.Descriptor, file *os.File) (cas.Object, error) {
	var result cas.Object
	err := retryPreparation(ctx, func(ctx context.Context) error {
		var err error
		result, err = p.objects.Publish(ctx, descriptor, file)
		return err
	})
	return result, err
}
