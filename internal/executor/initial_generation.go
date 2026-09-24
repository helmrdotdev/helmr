package executor

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type InitialGenerationClient interface {
	RegisterInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error
	CertifyInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error
}

// InitialGenerationPublisher binds object publication to one initial preparation.
// It cannot publish a continuation or advance the Computer head.
type InitialGenerationPublisher struct {
	client         InitialGenerationClient
	objects        computer.DiskPublisher
	runtimeID      string
	desiredVersion int64
}

func NewInitialGenerationPublisher(client InitialGenerationClient, objects computer.DiskPublisher, runtimeID string, desiredVersion int64) (*InitialGenerationPublisher, error) {
	if client == nil || objects == nil || runtimeID == "" || desiredVersion <= 0 {
		return nil, errors.New("initial generation publication dependencies and Runtime identity required")
	}
	return &InitialGenerationPublisher{client: client, objects: objects, runtimeID: runtimeID, desiredVersion: desiredVersion}, nil
}
func (p InitialGenerationPublisher) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	return p.client.RegisterInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{RuntimeInstanceID: p.runtimeID, DesiredVersion: p.desiredVersion, Inspection: e})
}
func (p InitialGenerationPublisher) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	return p.objects.Publish(ctx, d, file)
}
func (p InitialGenerationPublisher) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	return p.client.CertifyInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{RuntimeInstanceID: p.runtimeID, DesiredVersion: p.desiredVersion, Inspection: e})
}

var _ computer.GenerationPublication = InitialGenerationPublisher{}
