//go:build !linux

package firecracker

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

var ErrUnsupported = errors.New("firecracker connector is only supported on Linux")

type Connector struct{}

func NewConnector(Config) (*Connector, error) {
	return nil, ErrUnsupported
}

func (*Connector) Connect(context.Context, compute.NetworkPolicy) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*Connector) Restore(context.Context, vm.RestoreRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*Connector) RuntimeCapabilities() (RuntimeCapabilities, error) {
	return RuntimeCapabilities{}, ErrUnsupported
}

func (*Connector) Preflight(context.Context) error {
	return ErrUnsupported
}
