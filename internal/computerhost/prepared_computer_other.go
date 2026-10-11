//go:build !linux

package computerhost

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) prepareAllocationDevice(context.Context, workerapi.AllocationIdentity, workerapi.ComputerAllocationSource) (vm.ComputerDevice, error) {
	return nil, errors.New("Computer allocation materialization requires Linux")
}
