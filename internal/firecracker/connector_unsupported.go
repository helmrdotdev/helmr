//go:build !linux

package firecracker

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/vm"
)

var ErrUnsupported = errors.New("the Firecracker connector is only supported on Linux")

type Connector struct{}
type QualifiedRuntime struct{}

type NetworkReclaimer struct{}

func NewNetworkReclaimer(Config) (*NetworkReclaimer, error) { return nil, ErrUnsupported }

func (*NetworkReclaimer) Reclaim(context.Context, vm.Owner) error { return ErrUnsupported }

func NewConnector(Config) (*Connector, error) {
	return nil, ErrUnsupported
}

func (*Connector) Qualify(context.Context) (*QualifiedRuntime, error) {
	return nil, ErrUnsupported
}

func (*QualifiedRuntime) Restore(context.Context, vm.RestoreRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*QualifiedRuntime) Materialize(context.Context, vm.MaterializeRequest) (vm.Session, error) {
	return nil, ErrUnsupported
}

func (*QualifiedRuntime) Cleanup(context.Context, vm.Owner) error { return ErrUnsupported }

func (*QualifiedRuntime) RuntimeCapabilities() RuntimeCapabilities { return RuntimeCapabilities{} }

func (*QualifiedRuntime) HostRuntimeEvidence() HostRuntimeEvidence { return HostRuntimeEvidence{} }

func (*QualifiedRuntime) DatapathHealth() error { return ErrUnsupported }
