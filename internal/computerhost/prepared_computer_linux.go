//go:build linux

package computerhost

import (
	"context"
	"errors"

	"os"
	"path/filepath"

	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/nbd"

	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func (p *PreparedMachines) prepareAllocationDevice(ctx context.Context, identity workerapi.AllocationIdentity, source workerapi.ComputerAllocationSource) (vm.ComputerDevice, error) {
	material := source.Disk
	if err := material.Root.Validate(material.Root.LogicalBytes); err != nil {
		return nil, err
	}
	if len(material.Keys) == 0 {
		return nil, errors.New("computer allocation source has no keys")
	}
	keys := make(map[string][]byte, len(material.Keys))
	scope := material.Keys[0].Scope
	for _, key := range material.Keys {
		if key.Scope != scope || keys[key.ID] != nil {
			return nil, errors.New("computer allocation key scope or identity changed")
		}
		keys[key.ID] = key.Key
	}
	if _, err := disk.OpenVersion(ctx, p.ComputerRanges, scope, keys, material.Root, material.Root.LogicalBytes); err != nil {
		return nil, disk.PublishedSourceFailure(err)
	}
	dir := p.computerPreparationDirectory(identity.InstanceID, identity.Epoch)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, err
	}
	local, err := disk.CreateLocalVersion(ctx, disk.LocalVersionConfig{Directory: filepath.Join(dir, "version"), Base: material.Root, BaseSource: p.ComputerRanges, Scope: scope, ActiveKey: material.WriteKeyID, Keys: keys, DirtyBlocks: 256, StagedBytes: p.ComputerStagingBytes, PackLimit: blockformat.MinPackLimit})
	if err != nil {
		return nil, err
	}
	device, err := disk.AttachDevice(ctx, local, nbd.Config{Helper: p.ComputerHelper, Devices: p.ComputerDevices, Arena: dir, Socket: filepath.Join(dir, "nbd.sock"), Size: material.Root.LogicalBytes})
	if device != nil {
		p.retainComputerDevice(identity.InstanceID, identity.Epoch, device)
	}
	return device, err
}

// Retain before attachment can fail. Close is safe both before binding and after
// backend cleanup, but refuses release while a bound consumer may still live.
func (p *PreparedMachines) retainComputerDevice(id string, epoch int64, device vm.ComputerDevice) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.computerDevices == nil {
		p.computerDevices = make(map[preparedMachineRef]vm.ComputerDevice)
	}
	p.computerDevices[preparedMachineRef{id: id, epoch: epoch}] = device
}
