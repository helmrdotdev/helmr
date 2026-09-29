//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/filepack"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
)

func (runtime *QualifiedRuntime) Restore(ctx context.Context, request vm.RestoreRequest) (vm.Machine, error) {
	return runtime.connector.restore(ctx, request)
}

func (c *Connector) restore(ctx context.Context, request vm.RestoreRequest) (vm.Machine, error) {
	if err := request.Binding.Validate(vm.Owner{Kind: request.OwnerKind, ID: request.ComputerInstanceID}); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	targetRuntime, err := c.hostRuntime.vmPlatform()
	if err != nil {
		return nil, fmt.Errorf("resolve target host runtime identity: %w", err)
	}
	if _, err := c.hostRuntime.firecrackerExecutable(); err != nil {
		return nil, fmt.Errorf("resolve target Firecracker executable: %w", err)
	}
	if request.Binding.VMPlatformID != targetRuntime.ID {
		return nil, fmt.Errorf(
			"restore workload runtime identity %s does not match target host runtime %s",
			request.Binding.VMPlatformID,
			targetRuntime.ID,
		)
	}
	if len(request.Memory) != 1 {
		return nil, fmt.Errorf("the Firecracker restore requires exactly one memory file, got %d", len(request.Memory))
	}
	if len(request.MemoryMediaTypes) != 1 {
		return nil, fmt.Errorf("the Firecracker restore requires exactly one memory media type, got %d", len(request.MemoryMediaTypes))
	}
	if strings.TrimSpace(request.VMState) == "" {
		return nil, errors.New("the Firecracker restore vm state path is required")
	}
	if request.VMStateMediaType != cas.CheckpointVMStateMediaType {
		return nil, fmt.Errorf("the Firecracker restore vm state media type %q is not supported", request.VMStateMediaType)
	}
	recordPhase := request.RecordPhase
	started := time.Now()
	if err := validateReadOnlyDrives(request.ReadOnlyDrives); err != nil {
		return nil, err
	}
	if len(request.ReadOnlyDrives) != 0 && !isProgramDriveSet(request.ReadOnlyDrives) {
		return nil, errors.New("the Firecracker restore read-only drives must be the complete program drive set")
	}
	if isProgramDriveSet(request.ReadOnlyDrives) {
		if err := validateProgramDriveIdentities(request.ReadOnlyDrives); err != nil {
			return nil, err
		}
	}
	if err := validateComputerDisk(request.Topology.Computer); err != nil {
		return nil, err
	}
	kernelArgs := runtimeKernelArgs(request.Topology, request.ReadOnlyDrives, c.cfg.NetworkResolverIPv4)
	manifest, restoreCfg, err := c.validateRestoreIdentity(
		request.ID,
		request.Manifest,
		request.Checkpoint,
		request.Topology,
		kernelArgs,
		request.ReadOnlyDrives,
	)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_validate_identity", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.ScratchDisk) == "" {
		return nil, errors.New("the Firecracker restore scratch disk path is required")
	}
	if request.ScratchDiskMediaType != cas.CheckpointScratchDiskMediaType {
		return nil, fmt.Errorf("the Firecracker restore scratch disk media type %q is not supported", request.ScratchDiskMediaType)
	}
	if request.MemoryMediaTypes[0] != cas.CheckpointMemoryMediaType {
		return nil, fmt.Errorf("the Firecracker restore memory media type %q is not supported", request.MemoryMediaTypes[0])
	}
	if restoreCfg.MemoryMiB != request.Resources.MemoryMiB || restoreCfg.ScratchDiskMiB != request.Resources.DiskMiB {
		return nil, errors.New("checkpoint memory or scratch size does not match runtime reservation")
	}
	owner := vm.Owner{Kind: request.OwnerKind, ID: request.ComputerInstanceID}
	retained := c.lockComputerOwner(owner)
	if retained == nil {
		return nil, errors.New("runtime ownership is not configured")
	}
	transferred := false
	defer func() {
		if !transferred {
			retained.mu.Unlock()
		}
	}()
	ownerDir, err := createOwnerStateRoot(c.cfg.StateDir, owner)
	if err != nil {
		return nil, err
	}
	expectedScratchSize := manifest.RecoveryPoint.Runtime.ScratchDiskMiB * 1024 * 1024
	expectedMemorySize := manifest.RecoveryPoint.Runtime.MemoryMiB * 1024 * 1024
	var rawScratch string
	var rawMemory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, ownerDir, request.ScratchDisk, filepack.ScratchRole, scratchDiskName, expectedScratchSize, cas.CheckpointScratchDiskMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint scratch disk: %w", err)
		}
		rawScratch = path
		return nil
	})
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, ownerDir, request.Memory[0], filepack.MemoryRole, restoreMemoryName, expectedMemorySize, cas.CheckpointMemoryMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint memory: %w", err)
		}
		rawMemory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{rawScratch, rawMemory})
		return nil, errors.Join(err, removeStateRootLast(ownerDir, owner))
	}
	child := *c
	child.cfg = restoreCfg
	child.kernelArgs = kernelArgs
	transferred = true // prepareSession consumes the held restore guard.
	session, err := child.start(ctx, workloadLaunch, request.ComputerInstanceID, request.OwnerKind, request.Binding, rawMemory, request.VMState, rawScratch, &manifest.RuntimeState.Network, request.Topology, request.ReadOnlyDrives, recordPhase, retained)
	if err != nil {
		return nil, err
	}
	return session, nil
}

func (c *Connector) validateRestoreIdentity(
	checkpointID string,
	manifestBytes []byte,
	identity vm.CheckpointIdentity,
	topology vm.RuntimeTopology,
	kernelArgs string,
	readOnlyDrives []vm.ReadOnlyDrive,
) (snapshotManifest, Config, error) {
	var manifest snapshotManifest
	targetRuntime, err := c.hostRuntime.vmPlatform()
	if err != nil {
		return manifest, Config{}, fmt.Errorf("resolve target host runtime identity: %w", err)
	}
	if identity.RuntimeBackend != "firecracker" {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime backend %q is not supported", identity.RuntimeBackend)
	}
	workerArchitecture := targetRuntime.Arch
	if identity.RuntimeArch != workerArchitecture {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime arch %q does not match worker arch %q", identity.RuntimeArch, workerArchitecture)
	}
	if identity.VMRuntimeContract != targetRuntime.Contract {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime contract %q does not match worker contract %q", identity.VMRuntimeContract, targetRuntime.Contract)
	}
	if len(manifestBytes) == 0 {
		return manifest, Config{}, errors.New("checkpoint manifest is required")
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return manifest, Config{}, fmt.Errorf("decode checkpoint manifest: %w", err)
	}
	if manifest.RecoveryPoint.ID != checkpointID {
		return manifest, Config{}, fmt.Errorf("checkpoint manifest recovery point id %q does not match restore id %q", manifest.RecoveryPoint.ID, checkpointID)
	}
	kernelDigest := targetRuntime.KernelDigest
	if identity.KernelDigest != kernelDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint kernel digest %s does not match worker kernel digest %s", identity.KernelDigest, kernelDigest)
	}
	initramfsDigest := targetRuntime.InitramfsDigest
	if identity.InitramfsDigest != initramfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint initramfs digest %s does not match worker initramfs digest %s", identity.InitramfsDigest, initramfsDigest)
	}
	rootfsDigest := targetRuntime.RootfsDigest
	if identity.RootfsDigest != rootfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint rootfs digest %s does not match worker rootfs digest %s", identity.RootfsDigest, rootfsDigest)
	}
	if identity.VMConfigDigest != sha256sum.DigestBytes(manifestBytes) {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime config digest %s does not match checkpoint manifest digest %s", identity.VMConfigDigest, sha256sum.DigestBytes(manifestBytes))
	}
	runtimeID := targetRuntime.ID
	if identity.RuntimeID != runtimeID {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime id %s does not match worker runtime id %s", identity.RuntimeID, runtimeID)
	}
	if identity.VMVCPUCount <= 0 {
		return manifest, Config{}, fmt.Errorf("checkpoint VM vCPU count %d is invalid", identity.VMVCPUCount)
	}
	if int64(identity.VMVCPUCount) != manifest.RecoveryPoint.Runtime.VCPUCount {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint VM vCPU count %d does not match checkpoint manifest vCPU count %d",
			identity.VMVCPUCount,
			manifest.RecoveryPoint.Runtime.VCPUCount,
		)
	}
	if !sha256sum.ValidDigest(identity.CPUConfigDigest) {
		return manifest, Config{}, errors.New("checkpoint guest CPU configuration digest is not canonical")
	}
	if identity.CPUConfigDigest != manifest.RecoveryPoint.Runtime.CPUConfigDigest {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint guest CPU configuration digest %s does not match checkpoint manifest digest %s",
			identity.CPUConfigDigest,
			manifest.RecoveryPoint.Runtime.CPUConfigDigest,
		)
	}
	restoreCfg, err := c.configForRestoreManifest(manifest)
	if err != nil {
		return manifest, Config{}, err
	}
	targetCPUConfigDigest, err := c.hostRuntime.cpuConfigDigest(int64(identity.VMVCPUCount))
	if err != nil {
		return manifest, Config{}, fmt.Errorf("resolve target guest CPU configuration: %w", err)
	}
	if identity.CPUConfigDigest != targetCPUConfigDigest {
		return manifest, Config{}, fmt.Errorf(
			"checkpoint guest CPU configuration digest %s does not match target digest %s for %d vCPUs",
			identity.CPUConfigDigest,
			targetCPUConfigDigest,
			identity.VMVCPUCount,
		)
	}
	if err := validateRuntimeManifest(
		restoreCfg,
		manifest,
		runtimeID,
		kernelDigest,
		initramfsDigest,
		rootfsDigest,
		identity.CPUConfigDigest,
		kernelArgs,
		readOnlyDrives,
	); err != nil {
		return manifest, Config{}, err
	}
	return manifest, restoreCfg, nil
}

func (c *Connector) configForRestoreManifest(manifest snapshotManifest) (Config, error) {
	cfg := c.cfg
	runtimeManifest := manifest.RecoveryPoint.Runtime
	if runtimeManifest.VCPUCount <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest vcpu count %d is invalid", runtimeManifest.VCPUCount)
	}
	if runtimeManifest.MemoryMiB <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest memory %d MiB is invalid", runtimeManifest.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB <= 0 {
		return Config{}, fmt.Errorf("checkpoint manifest scratch disk size %d MiB is invalid", runtimeManifest.ScratchDiskMiB)
	}
	if runtimeManifest.VCPUCount > cfg.VCPUCount {
		return Config{}, fmt.Errorf("checkpoint manifest vcpu count %d exceeds worker capacity %d", runtimeManifest.VCPUCount, cfg.VCPUCount)
	}
	if runtimeManifest.MemoryMiB > cfg.MemoryMiB {
		return Config{}, fmt.Errorf("checkpoint manifest memory %d MiB exceeds worker capacity %d MiB", runtimeManifest.MemoryMiB, cfg.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB > cfg.ScratchDiskMiB {
		return Config{}, fmt.Errorf("checkpoint manifest scratch disk size %d MiB exceeds worker capacity %d MiB", runtimeManifest.ScratchDiskMiB, cfg.ScratchDiskMiB)
	}
	cfg.VCPUCount = runtimeManifest.VCPUCount
	cfg.MemoryMiB = runtimeManifest.MemoryMiB
	cfg.ScratchDiskMiB = runtimeManifest.ScratchDiskMiB
	return cfg, nil
}

func (c *Connector) unpackRestoreArtifact(ctx context.Context, ownerDir string, artifactPath string, role string, suffix string, expectedLogicalSize int64, mediaType string) (string, vm.RuntimePhase, error) {
	started := time.Now()
	phase := vm.RuntimePhase{
		Name:      "restore_unpack_" + strings.ReplaceAll(role, "-", "_") + "_filepack",
		Role:      role,
		MediaType: mediaType,
	}
	if role == filepack.ScratchRole {
		phase.Name = "restore_unpack_scratch_filepack"
	}
	file, err := os.CreateTemp(ownerDir, "restore-*."+suffix)
	if err != nil {
		phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	targetPath := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(targetPath)
		phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	_ = os.Remove(targetPath)
	stats, err := filepack.Unpack(ctx, artifactPath, targetPath, role, expectedLogicalSize)
	phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
	if err == nil || stats.LogicalBytes != 0 || stats.EncodedChunks != 0 || stats.UnpackWrittenBytes != 0 {
		measured := vm.FilepackStats(stats)
		phase.Filepack = &measured
	}
	if err != nil {
		_ = os.Remove(targetPath)
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	return targetPath, phase, nil
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func withSnapshotRestore(memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		firecracker.WithSnapshot(memoryPath, statePath, func(config *firecracker.SnapshotConfig) {
			config.EnableDiffSnapshots = CanonicalVMRuntimeDescriptor().Snapshot.LoadEnableDiffSnapshots
			config.ResumeVM = CanonicalVMRuntimeDescriptor().Snapshot.LoadResumeVM
		})(machine)
		machine.Handlers.FcInit = machine.Handlers.FcInit.Remove(firecracker.AddVsocksHandlerName)
	}
}
