//go:build !linux

package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) prepareComputerDevice(context.Context, workerapi.InstanceReconcileTarget) (vm.ComputerDevice, error) {
	return nil, errors.New("computer runtime preparation requires Linux")
}
