//go:build !linux

package executor

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedRuntimePool) prepareComputerDevice(context.Context, workerapi.RuntimeReconcileTarget) (vm.ComputerDevice, error) {
	return nil, errors.New("computer runtime preparation requires Linux")
}
