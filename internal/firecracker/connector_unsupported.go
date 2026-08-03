//go:build !linux

package firecracker

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/vm"
)

var ErrUnsupported = errors.New("the Firecracker connector is only supported on Linux")

type Connector struct{}

type NetworkReclaimer struct{}

func NewNetworkReclaimer(Config) (*NetworkReclaimer, error) { return nil, ErrUnsupported }

func (*NetworkReclaimer) Reclaim(context.Context, vm.Owner) error { return ErrUnsupported }

func NewConnector(Config) (*Connector, error) {
	return nil, ErrUnsupported
}

func (*Connector) Connect(context.Context, vm.ConnectRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*Connector) Restore(context.Context, vm.RestoreRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*Connector) Materialize(context.Context, vm.MaterializeRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*Connector) Cleanup(context.Context, vm.Owner) error { return ErrUnsupported }

func (*Connector) RuntimeCapabilities() (RuntimeCapabilities, error) {
	return RuntimeCapabilities{}, ErrUnsupported
}

func (*Connector) Preflight(context.Context) error {
	return ErrUnsupported
}

func (*Connector) DatapathHealth() error { return ErrUnsupported }
