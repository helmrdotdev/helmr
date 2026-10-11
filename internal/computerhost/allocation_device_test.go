package computerhost

import (
	"context"
	"sync"

	"github.com/helmrdotdev/helmr/internal/vm"
)

type countingCloseComputerDevice struct {
	vm.ComputerDevice
	mu     sync.Mutex
	err    error
	closes int
}

func (d *countingCloseComputerDevice) Close(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closes++
	return d.err
}

func (d *countingCloseComputerDevice) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

func computerDeviceTracked(machines *PreparedMachines, ref preparedMachineRef) bool {
	machines.mu.Lock()
	defer machines.mu.Unlock()
	return machines.computerDevices[ref] != nil
}
