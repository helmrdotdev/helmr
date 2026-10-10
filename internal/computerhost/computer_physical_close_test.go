package computerhost

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/vm"
)

type failingCloseComputerDevice struct {
	vm.ComputerDevice
	err error
}

func (d *failingCloseComputerDevice) Close(context.Context) error { return d.err }
