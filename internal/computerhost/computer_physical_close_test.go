package computerhost

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/disk"

	"github.com/helmrdotdev/helmr/internal/vm"
)

type failingCloseComputerDevice struct {
	vm.ComputerDevice
	err     error
	changes func() disk.VersionChanges
}

func (d *failingCloseComputerDevice) Close(context.Context) error { return d.err }

func (d *failingCloseComputerDevice) Changes() disk.VersionChanges {
	if d.changes != nil {
		return d.changes()
	}
	return disk.VersionChanges{}
}
