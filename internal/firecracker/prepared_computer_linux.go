//go:build linux

package firecracker

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"sync"

	"github.com/helmrdotdev/helmr/internal/firecracker/custody"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type preparedComputer struct {
	mu        sync.Mutex
	machine   *guestMachine
	connector *Connector
	epoch     int64
	attempted bool
}

func (runtime *QualifiedRuntime) PrepareComputer(ctx context.Context, request vm.MaterializeRequest) (vm.PreparedComputer, error) {
	c := runtime.connector
	if request.BootSeed == nil || request.Purpose != vm.MaterializeWorkload {
		return nil, errors.New("prepared Computer requires a qualified boot seed")
	}
	if err := c.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	cfg, err := c.configForMaterializeRequest(request)
	if err != nil {
		return nil, err
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, request.ReadOnlyDrives, c.cfg.NetworkResolverIPv4)
	machine, err := child.restoreBootSeedMode(ctx, request, bootSeedBuildLaunch)
	if err != nil {
		return nil, err
	}
	return &preparedComputer{machine: machine.(*guestMachine), connector: &child, epoch: request.Binding.WorkerEpoch}, nil
}

func (p *preparedComputer) Close(ctx context.Context) error {
	p.mu.Lock()
	p.attempted = true
	p.mu.Unlock()
	return p.machine.Close(ctx)
}

func (p *preparedComputer) Adopt(ctx context.Context, request vm.PreparedAdoption) (vm.CheckpointableMachine, error) {
	binding, computerID := request.Binding, request.ComputerID
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempted {
		return nil, errors.New("prepared Computer already claimed or closed")
	}
	p.attempted = true
	s := p.machine
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, errors.New("prepared Computer is closed")
	}
	logical := vm.Owner{Kind: vm.OwnerInstance, ID: binding.OwnerID}
	if err := logical.Validate(); err != nil {
		return nil, err
	}
	if err := binding.Validate(logical); err != nil {
		return nil, err
	}
	if binding.WorkerEpoch != p.epoch || binding.VMPlatformID != s.vmPlatform.ID || logical.ID == s.owner.ID || ids.Validate(computerID) != nil {
		return nil, errors.New("prepared Computer adoption identity differs")
	}
	if s.topology.Computer == nil || s.topology.Computer.Device.Changes().Generation != 0 {
		return nil, errors.New("prepared Computer crossed its disk gate")
	}
	// Record the logical allocation before any credential can reach this VM.
	// A crash or partial network update retains this allocation in quarantine.
	if err := (custody.StateRoot{Base: p.connector.cfg.StateDir}).RecordPreparedAdoption(custody.PreparedAdoption{Owner: s.owner, AllocationID: logical.ID, WorkerEpoch: p.epoch}); err != nil {
		return nil, err
	}
	device, ok := s.topology.Computer.Device.(interface {
		AdoptPrepared(context.Context, disk.VersionRoot, string, map[string][]byte, blockformat.RangeSource) error
	})
	if !ok {
		return nil, errors.New("prepared disk cannot adopt allocation authority")
	}
	if err := device.AdoptPrepared(ctx, request.Root, request.WriteKeyID, request.Keys, request.Source); err != nil {
		return nil, err
	}
	network := s.networkBinding
	if network == nil {
		return nil, errors.New("prepared Computer network is missing")
	}
	network.mu.Lock()
	err = network.verifyLocked(true)
	if err == nil && network.secretProxy != nil {
		err = errors.New("prepared Computer already has Secret transport")
	}
	if err == nil {
		err = p.connector.installRoutedPolicyFor(ctx, workloadLaunch, network, logical.ID, true)
	}
	if err == nil {
		err = p.connector.persistInstalledNetworkOwner(network)
	}
	if err == nil {
		err = network.verifyLocked(true)
	}
	network.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.topology.Computer.ComputerID = computerID
	s.bootstrap, err = p.connector.bootstrapGuest(ctx, s.vsockHostPath, s.machineExit, false)
	if err != nil {
		return nil, err
	}
	if err := p.connector.waitForHealth(ctx, s.vsockHostPath, s.machineExit, s.machine.Logger().Printf); err != nil {
		return nil, err
	}
	if _, err := s.Open(ctx); err != nil {
		return nil, err
	}
	return s, nil
}
