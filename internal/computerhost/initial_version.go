package computerhost

import (
	"context"
	"errors"
	"os"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type InitialVersionClient interface {
	RegisterInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error
	CertifyInitialComputerObject(context.Context, workerapi.InitialComputerObjectRequest) error
}

// InitialVersionPublisher binds object publication to one initial preparation.
// It cannot publish a continuation or advance the Computer head.
type InitialVersionPublisher struct {
	client         InitialVersionClient
	objects        versionObjectPublisher
	runtimeID      string
	desiredVersion int64
}

func NewInitialVersionPublisher(client InitialVersionClient, objects versionObjectPublisher, runtimeID string, desiredVersion int64) (*InitialVersionPublisher, error) {
	if client == nil || objects == nil || runtimeID == "" || desiredVersion <= 0 {
		return nil, errors.New("initial version publication dependencies and Runtime identity required")
	}
	return &InitialVersionPublisher{client: client, objects: objects, runtimeID: runtimeID, desiredVersion: desiredVersion}, nil
}
func (p InitialVersionPublisher) Register(ctx context.Context, e blockformat.ObjectInspection) error {
	return p.client.RegisterInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{ComputerInstanceID: p.runtimeID, DesiredVersion: p.desiredVersion, Inspection: e})
}
func (p InitialVersionPublisher) Upload(ctx context.Context, d cas.Descriptor, file *os.File) (cas.Object, error) {
	return p.objects.Publish(ctx, d, file)
}
func (p InitialVersionPublisher) Certify(ctx context.Context, e blockformat.ObjectInspection) error {
	return p.client.CertifyInitialComputerObject(ctx, workerapi.InitialComputerObjectRequest{ComputerInstanceID: p.runtimeID, DesiredVersion: p.desiredVersion, Inspection: e})
}

var _ disk.VersionPublication = InitialVersionPublisher{}

type versionObjectPublisher interface {
	Publish(context.Context, cas.Descriptor, *os.File) (cas.Object, error)
}
