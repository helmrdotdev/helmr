//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/firecracker-microvm/firecracker-go-sdk/client/operations"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/filepack"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type bootSeedManifest struct {
	Contract      string                   `json:"contract"`
	RuntimeID     string                   `json:"runtime_id"`
	CPUConfig     string                   `json:"cpu_config"`
	VCPUs         int64                    `json:"vcpus"`
	MemoryMiB     int64                    `json:"memory_mib"`
	ScratchMiB    int64                    `json:"scratch_mib"`
	ComputerRoot  string                   `json:"computer_root"`
	ComputerBytes int64                    `json:"computer_bytes"`
	KernelArgs    string                   `json:"kernel_args"`
	Program       *snapshotProgramManifest `json:"program"`
	Network       snapshotNetworkManifest  `json:"network"`
	GuestPort     uint32                   `json:"guest_port"`
	HealthPort    uint32                   `json:"health_port"`
}

func (c *Connector) bootSeedManifest(request vm.MaterializeRequest) ([]byte, error) {
	if request.Topology.Computer == nil || request.Topology.Computer.Device == nil ||
		!sha256sum.ValidDigest(request.Topology.Computer.VersionID) ||
		request.BaseComputerDiskVersionID != request.Topology.Computer.VersionID {
		return nil, errors.New("boot seed requires the exact prepared root and its owned device")
	}
	program, err := snapshotProgram(request.ReadOnlyDrives)
	if err != nil {
		return nil, err
	}
	network := snapshotNetworkConfig(c.cfg)
	if err := validateSnapshotNetwork(network); err != nil {
		return nil, err
	}
	return json.Marshal(bootSeedManifest{
		Contract: "helmr.boot-seed.v1", RuntimeID: request.Binding.VMPlatformID,
		CPUConfig: request.CPUConfigDigest, VCPUs: int64(request.VMVCPUCount),
		MemoryMiB: request.Resources.MemoryMiB, ScratchMiB: request.Resources.DiskMiB,
		ComputerRoot: request.Topology.Computer.VersionID, ComputerBytes: request.Topology.Computer.SizeBytes,
		KernelArgs: runtimeKernelArgs(request.Topology, request.ReadOnlyDrives, c.cfg.NetworkResolverIPv4),
		Program:    program, Network: network, GuestPort: c.cfg.GuestPort, HealthPort: c.cfg.HealthPort,
	})
}

type bootSeedCapture struct {
	mu        sync.Mutex
	machine   *guestMachine
	manifest  []byte
	attempted bool
	closed    bool
}

func (s *guestMachine) BootSeedIdentity() vm.BootSeedIdentity {
	return vm.BootSeedIdentity{BootID: s.bootstrap.BootID, RandomDigest: sha256sum.DigestBytes(s.bootstrap.Random[:])}
}

func (runtime *QualifiedRuntime) BootSeedMatches(request vm.MaterializeRequest, manifest []byte) (bool, error) {
	if err := runtime.connector.validateMaterializeRequest(request); err != nil {
		return false, err
	}
	expected, err := runtime.connector.bootSeedManifest(request)
	return bytes.Equal(expected, manifest), err
}

func (runtime *QualifiedRuntime) PrepareBootSeed(ctx context.Context, request vm.MaterializeRequest) (vm.BootSeedCapture, error) {
	c := runtime.connector
	if request.BootSeed != nil || request.Purpose != vm.MaterializeWorkload {
		return nil, errors.New("boot seed builder requires a fresh private boot")
	}
	if err := c.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	manifest, err := c.bootSeedManifest(request)
	if err != nil {
		return nil, err
	}
	if request.Topology.Computer.Device.Changes().Generation != 0 {
		return nil, errors.New("boot seed input already contains private writes")
	}
	cfg, err := c.configForMaterializeRequest(request)
	if err != nil {
		return nil, err
	}
	child := *c
	child.cfg = cfg
	child.kernelArgs = runtimeKernelArgs(request.Topology, request.ReadOnlyDrives, c.cfg.NetworkResolverIPv4)
	machine, err := child.prepareMachine(ctx, bootSeedBuildLaunch, request.ID, request.OwnerKind, request.Binding,
		"", "", "", nil, request.Topology, request.ReadOnlyDrives, request.RecordPhase, nil)
	if err != nil {
		return nil, err
	}
	return &bootSeedCapture{machine: machine, manifest: manifest}, nil
}

func (b *bootSeedCapture) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	return b.machine.Close(ctx)
}

func (b *bootSeedCapture) Capture(ctx context.Context) (vm.BootSeedArtifact, error) {
	b.mu.Lock()
	if b.attempted || b.closed {
		b.mu.Unlock()
		return vm.BootSeedArtifact{}, errors.New("boot seed capture is no longer available")
	}
	b.attempted = true
	b.mu.Unlock()
	s := b.machine
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return vm.BootSeedArtifact{}, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		unlock()
		return vm.BootSeedArtifact{}, errors.New("boot seed builder is closed")
	}
	captureCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.computerCancel = cancel
	s.mu.Unlock()
	type outcome struct {
		artifact vm.BootSeedArtifact
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		artifact, err := b.capture(captureCtx)
		cancel()
		s.mu.Lock()
		s.computerCancel = nil
		s.mu.Unlock()
		unlock()
		done <- outcome{artifact, err}
	}()
	select {
	case result := <-done:
		return result.artifact, errors.Join(result.err, ctx.Err())
	case <-ctx.Done():
		return vm.BootSeedArtifact{}, ctx.Err()
	}
}

func (b *bootSeedCapture) capture(ctx context.Context) (vm.BootSeedArtifact, error) {
	var result vm.BootSeedArtifact
	s := b.machine
	if err := s.machine.PauseVM(ctx, func(params *operations.PatchVMParams) { params.SetContext(ctx) }); err != nil {
		return result, err
	}
	backings, err := s.inspectPausedDiskBackings(ctx)
	if err != nil {
		return result, err
	}
	const memoryName, stateName = "boot-seed.mem", "boot-seed.vmstate"
	if err := captureSnapshotState(ctx, s.machine.Cfg.SocketPath, s.jailRoot, memoryName, stateName, s.cfg.JailerUID, s.cfg.JailerGID); err != nil {
		return result, err
	}
	if err := s.syncPausedDiskBackings(ctx, backings); err != nil {
		return result, err
	}
	// The block backend counts every guest mutation, even one that restores
	// identical bytes. No post-boot root is substituted for the prepared root.
	if s.topology.Computer.Device.Changes().Generation != 0 {
		return result, errors.New("boot seed crossed the pre-mount Computer boundary")
	}
	scratch, _, err := s.packSnapshotRuntimeFile(ctx, s.scratchDisk, filepack.ScratchRole, "boot-seed.scratch.filepack", cas.BootSeedScratchMediaType)
	if err != nil {
		return result, err
	}
	result = vm.BootSeedArtifact{
		Manifest:    append([]byte(nil), b.manifest...),
		VMState:     vm.SnapshotFile{Path: filepath.Join(s.jailRoot, stateName), MediaType: cas.BootSeedVMStateMediaType},
		ScratchDisk: scratch,
		Memory:      vm.SnapshotFile{Path: filepath.Join(s.jailRoot, memoryName), MediaType: cas.BootSeedMemoryMediaType},
		MemoryBytes: s.cfg.MemoryMiB * (1 << 20),
	}
	return result, nil
}

func (c *Connector) restoreBootSeed(ctx context.Context, request vm.MaterializeRequest) (vm.CheckpointableMachine, error) {
	seed := request.BootSeed
	expected, err := c.bootSeedManifest(request)
	if err != nil {
		return nil, err
	}
	if seed == nil || !bytes.Equal(seed.Manifest, expected) {
		return nil, errors.New("boot seed content or runtime does not match materialization")
	}
	if seed.MemorySource == nil || seed.MemorySource.Size() != request.Resources.MemoryMiB*(1<<20) ||
		!filepath.IsAbs(seed.VMState) || !filepath.IsAbs(seed.ScratchDisk) {
		return nil, errors.New("boot seed requires authenticated memory, state and scratch")
	}
	if err := validateBootSeedState(seed.VMState); err != nil {
		return nil, err
	}
	if request.Topology.Computer.Device.Changes().Generation != 0 {
		return nil, errors.New("boot seed target already contains private writes")
	}
	owner := vm.Owner{Kind: request.OwnerKind, ID: request.ID}
	retained := c.lockComputerOwner(owner)
	if retained == nil {
		return nil, errors.New("boot seed runtime ownership is not configured")
	}
	transferred := false
	defer func() {
		if !transferred {
			retained.mu.Unlock()
		}
	}()
	directory, err := createOwnerStateRoot(c.cfg.StateDir, owner)
	if err != nil {
		return nil, err
	}
	scratch, phase, err := c.unpackRestoreArtifact(ctx, directory, seed.ScratchDisk, filepack.ScratchRole, scratchDiskName, request.Resources.DiskMiB*(1<<20), cas.BootSeedScratchMediaType)
	reportPhase(request.RecordPhase, phase)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("unpack boot seed scratch: %w", err), removeStateRootLast(directory, owner))
	}
	child := *c
	child.restoreMemory = seed.MemorySource
	child.seedRestore = true
	network := snapshotNetworkConfig(c.cfg)
	mode := workloadLaunch
	if request.Purpose == vm.MaterializePreparationQualification {
		mode = preparationQualificationLaunch
	}
	transferred = true
	machine, err := child.start(ctx, mode, request.ID, request.OwnerKind, request.Binding,
		"", seed.VMState, scratch, &network, request.Topology, request.ReadOnlyDrives, request.RecordPhase, retained)
	if err != nil {
		// start owns physical cleanup; no fallback or second launch is allowed.
		return nil, err
	}
	return machine, nil
}

// Seed state must remain bounded before it reaches the VMM's parser.
func validateBootSeedState(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > vm.SnapshotStateLimit {
		return errors.New("boot seed state size is invalid")
	}
	return nil
}
