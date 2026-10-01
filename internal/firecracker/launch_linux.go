//go:build linux

package firecracker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
)

const ext4SuperblockOffset = 1024
const ext4SuperblockBytes = 1024
const ext4Magic = 0xef53

var nextGuestCID atomic.Uint32

func (c *Connector) validateMaterializeRequest(request vm.MaterializeRequest) error {
	if request.Topology.Computer != nil {
		if err := validateComputerDisk(request.Topology.Computer); err != nil {
			return err
		}
	}
	if request.OwnerKind != vm.OwnerInstance {
		return errors.New("the Firecracker materialize owner must be an Instance")
	}
	if err := request.Binding.Validate(vm.Owner{Kind: request.OwnerKind, ID: request.ID}); err != nil {
		return fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	if err := validateReadOnlyDrives(request.ReadOnlyDrives); err != nil {
		return err
	}
	if len(request.ReadOnlyDrives) != 0 && !isProgramDriveSet(request.ReadOnlyDrives) {
		return errors.New("runtime read-only drives must be the complete program drive set")
	}
	if isProgramDriveSet(request.ReadOnlyDrives) {
		if err := validateProgramDriveIdentities(request.ReadOnlyDrives); err != nil {
			return err
		}
	}
	rootfsDigest := c.artifacts.Rootfs.Digest
	if rootfsDigest != strings.TrimSpace(request.RootfsDigest) {
		return fmt.Errorf("computerMount rootfs digest %s does not match declared digest %s", rootfsDigest, request.RootfsDigest)
	}
	if strings.TrimSpace(request.ComputerMountPath) != "/workspace" {
		return fmt.Errorf("the Firecracker materialize computer mount path %q is not supported", request.ComputerMountPath)
	}
	requestedVCPUs, err := VCPUCountForMilliCPU(request.Resources.MilliCPU)
	if err != nil {
		return fmt.Errorf("derive materialize VM vCPU count: %w", err)
	}
	if request.VMVCPUCount <= 0 || int64(request.VMVCPUCount) != requestedVCPUs {
		return fmt.Errorf(
			"materialize VM vCPU count %d does not match %d milliCPU-derived vCPUs %d",
			request.VMVCPUCount,
			request.Resources.MilliCPU,
			requestedVCPUs,
		)
	}
	if !sha256sum.ValidDigest(request.CPUConfigDigest) {
		return errors.New("materialize guest CPU configuration digest is not canonical")
	}
	targetRuntime, targetCPUConfigDigest, _, err := c.boundMachineRuntime(requestedVCPUs)
	if err != nil {
		return fmt.Errorf("resolve materialize host runtime: %w", err)
	}
	if request.Binding.VMPlatformID != targetRuntime.ID {
		return fmt.Errorf(
			"materialize runtime identity %s does not match target host runtime %s",
			request.Binding.VMPlatformID,
			targetRuntime.ID,
		)
	}
	if request.CPUConfigDigest != targetCPUConfigDigest {
		return fmt.Errorf(
			"materialize guest CPU configuration digest %s does not match target digest %s for %d vCPUs",
			request.CPUConfigDigest,
			targetCPUConfigDigest,
			requestedVCPUs,
		)
	}
	return nil
}

func runtimeKernelArgs(
	topology vm.Topology,
	readOnlyDrives []vm.ReadOnlyDrive,
	resolverIPv4 string,
) string {
	guestIP, guestNetwork, _ := net.ParseCIDR(GuestNetworkCIDRV0)
	// Root init configures the interface after initramfs has loaded virtio_net.
	// Giving this configuration to the SDK would also enable kernel IP autoconfiguration.
	args := defaultKernelArgs + fmt.Sprintf(" %s=%s::%s:%s::%s:off:%s::",
		runtimeIPKernelParameter, guestIP, GuestGatewayIPv4V0,
		net.IP(guestNetwork.Mask), GuestInterfaceNameV0, strings.TrimSpace(resolverIPv4))
	if topology.Computer != nil {
		args += " helmr.computer=1"
	}
	if isProgramDriveSet(readOnlyDrives) {
		args += " " + runtimeProgramKernelFlag
	}
	return args
}

func (c *Connector) configForMaterializeRequest(request vm.MaterializeRequest) (Config, error) {
	return c.configForResources(request.Resources, "materialize")
}

func (c *Connector) configForResources(resources vm.Resources, operation string) (Config, error) {
	cfg := c.cfg
	if err := resources.Validate(); err != nil {
		return Config{}, fmt.Errorf("%s resources: %w", operation, err)
	}
	if resources.MemoryMiB > 0 {
		if resources.MemoryMiB > cfg.MemoryMiB {
			return Config{}, fmt.Errorf("%s requested memory %d MiB exceeds worker VM memory capacity %d MiB", operation, resources.MemoryMiB, cfg.MemoryMiB)
		}
		cfg.MemoryMiB = resources.MemoryMiB
	}
	if resources.MilliCPU > 0 {
		requestedVCPUs, err := VCPUCountForMilliCPU(resources.MilliCPU)
		if err != nil {
			return Config{}, fmt.Errorf("%s requested cpu: %w", operation, err)
		}
		if requestedVCPUs > cfg.VCPUCount {
			return Config{}, fmt.Errorf("%s requested cpu %d milliCPU exceeds worker VM vCPU capacity %d", operation, resources.MilliCPU, cfg.VCPUCount)
		}
		cfg.VCPUCount = requestedVCPUs
	}
	if resources.DiskMiB > 0 {
		if resources.DiskMiB > cfg.ScratchDiskMiB {
			return Config{}, fmt.Errorf("%s requested disk %d MiB exceeds worker VM scratch disk capacity %d MiB", operation, resources.DiskMiB, cfg.ScratchDiskMiB)
		}
		cfg.ScratchDiskMiB = resources.DiskMiB
	}
	return cfg, nil
}

func (c *Connector) kernelArgsValue() string {
	if strings.TrimSpace(c.kernelArgs) == "" {
		return runtimeKernelArgs(vm.Topology{}, nil, c.cfg.NetworkResolverIPv4)
	}
	return c.kernelArgs
}

func (c *Connector) start(ctx context.Context, mode launchMode, instanceID string, ownerKind vm.OwnerKind, binding vm.WorkloadBinding, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest, topology vm.Topology, readOnlyDrives []vm.ReadOnlyDrive, recordPhase func(vm.Phase), preparedOwner *computerDeviceOwner) (vm.CheckpointableMachine, error) {
	machine, err := c.prepareMachine(ctx, mode, instanceID, ownerKind, binding, snapshotMemoryPath, snapshotStatePath, scratchDiskRestorePath, restoreNetwork, topology, readOnlyDrives, recordPhase, preparedOwner)
	if err != nil {
		return nil, err
	}
	if _, err := machine.Open(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()
		return nil, errors.Join(err, machine.Close(cleanupCtx))
	}
	return machine, nil
}

func (c *Connector) prepareMachine(ctx context.Context, mode launchMode, instanceID string, ownerKind vm.OwnerKind, binding vm.WorkloadBinding, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest, topology vm.Topology, readOnlyDrives []vm.ReadOnlyDrive, recordPhase func(vm.Phase), preparedOwner *computerDeviceOwner) (_ *guestMachine, retErr error) {
	if preparedOwner != nil {
		defer preparedOwner.mu.Unlock()
	}
	if err := validateCPUTemplateLaunch(c.cfg.CPUTemplateSelector); err != nil {
		return nil, err
	}
	vmPlatform, cpuConfigDigest, firecrackerPath, err := c.boundMachineRuntime(c.cfg.VCPUCount)
	if err != nil {
		return nil, err
	}
	launchCfg := c.cfg
	launchCfg.FirecrackerPath = firecrackerPath
	instanceID = strings.TrimSpace(instanceID)
	owner := vm.Owner{Kind: ownerKind, ID: instanceID}
	if err := owner.Validate(); err != nil {
		return nil, fmt.Errorf("the Firecracker owner: %w", err)
	}
	if err := binding.Validate(owner); err != nil {
		return nil, fmt.Errorf("the Firecracker workload binding: %w", err)
	}
	if binding.VMPlatformID != vmPlatform.ID {
		return nil, fmt.Errorf(
			"workload runtime identity %s does not match bound host runtime %s",
			binding.VMPlatformID,
			vmPlatform.ID,
		)
	}
	retained := preparedOwner
	if retained == nil {
		retained = c.lockComputerOwner(owner)
		if retained != nil {
			defer retained.mu.Unlock()
		}
	}
	instanceDir := filepath.Join(c.cfg.StateDir, instanceID)
	if preparedOwner != nil {
		if err := validateOwnerMarker(instanceDir, owner); err != nil {
			return nil, fmt.Errorf("validate prepared Firecracker ownership evidence: %w", err)
		}
	} else {
		var err error
		instanceDir, err = createOwnerStateRoot(c.cfg.StateDir, owner)
		if err != nil {
			return nil, err
		}
	}
	defer func() {
		if retErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			retErr = errors.Join(retErr, c.cleanupOwned(cleanupCtx, owner, retained))
		}
	}()
	if topology.Computer != nil && topology.Computer.Device != nil {
		if err := retainComputerDevice(retained, topology.Computer.Device); err != nil {
			return nil, err
		}
	}
	scratchDiskPath := filepath.Join(instanceDir, scratchDiskName)
	if strings.TrimSpace(scratchDiskRestorePath) != "" {
		scratchDiskPath = scratchDiskRestorePath
	} else {
		phaseStarted := time.Now()
		err := c.createScratchDisk(ctx, scratchDiskPath)
		reportPhase(recordPhase, vm.Phase{Name: "materialize_create_scratch_disk", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		if err != nil {
			return nil, err
		}
	}
	phaseStarted := time.Now()
	if err := c.prepareScratchDiskForJailer(scratchDiskPath); err != nil {
		reportPhase(recordPhase, vm.Phase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		return nil, err
	}
	reportPhase(recordPhase, vm.Phase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	restoring := snapshotMemoryPath != "" || snapshotStatePath != ""
	computerDiskPath := ""
	if topology.Computer != nil {
		computerDiskPath, err = attachComputerDisk(ctx, topology.Computer, instanceDir, c.cfg.JailerUID, c.cfg.JailerGID)
		if err != nil {
			return nil, err
		}
		copy := *topology.Computer
		// Keep the live publication owner; cloneComputerDisk strips it from persisted snapshots.
		copy.Path, copy.File = computerDiskPath, nil
		topology.Computer = &copy
	}
	readOnlyDrivePaths := map[string]string(nil)
	if restoring && len(readOnlyDrives) != 0 {
		readOnlyDrivePaths, err = prepareRestoreReadOnlyDrivePaths(
			instanceDir,
			readOnlyDrives,
			c.cfg.JailerUID,
			c.cfg.JailerGID,
		)
		if err != nil {
			return nil, err
		}
	}
	jailRoot := jailRootPath(launchCfg, instanceID)

	vsockHostPath := filepath.Join(jailRoot, vsockSocketName)
	guestCID := allocateGuestCID()
	var chrootStrategy firecracker.HandlersAdapter = firecracker.NewNaiveChrootStrategy(
		c.cfg.KernelPath,
	)
	if len(readOnlyDrives) != 0 {
		chrootStrategy = sealedDriveChrootStrategy{
			kernelImagePath: c.cfg.KernelPath,
			drives:          readOnlyDrives,
		}
	}
	runtimeDescriptor := CanonicalVMRuntimeDescriptor()
	machineCfg := firecracker.Config{
		VMID:            instanceID,
		SocketPath:      apiSocketName,
		LogLevel:        "Info",
		KernelImagePath: c.cfg.KernelPath,
		InitrdPath:      c.cfg.InitramfsPath,
		KernelArgs:      c.kernelArgsValue(),
		Seccomp: firecracker.SeccompConfig{
			Enabled: true,
		},
		JailerCfg: &firecracker.JailerConfig{
			UID:            firecracker.Int(c.cfg.JailerUID),
			GID:            firecracker.Int(c.cfg.JailerGID),
			ID:             instanceID,
			NumaNode:       firecracker.Int(c.cfg.JailerNumaNode),
			ExecFile:       launchCfg.FirecrackerPath,
			JailerBinary:   c.cfg.JailerPath,
			ChrootBaseDir:  c.cfg.JailerChrootBaseDir,
			ChrootStrategy: chrootStrategy,
			CgroupVersion:  c.cfg.CgroupVersion,
			Stdin:          nil,
			Stdout:         os.Stderr,
			Stderr:         os.Stderr,
		},
		Drives: runtimeDrivesWithComputer(
			c.cfg.RootfsPath,
			scratchDiskPath,
			computerDiskPath,
			readOnlyDrives,
			readOnlyDrivePaths,
		),
		VsockDevices: []firecracker.VsockDevice{runtimeVsockDevice(runtimeDescriptor, guestCID)},
		MachineCfg:   runtimeMachineConfiguration(runtimeDescriptor, c.cfg),
	}
	machineCfg.NetNS = filepath.Join("/var/run/netns", instanceID)
	machineCfg.NetworkInterfaces = firecracker.NetworkInterfaces{staticNetworkInterface()}
	var networkBinding *installedNetworkBinding
	defer func() {
		if retErr != nil && networkBinding != nil {
			retErr = errors.Join(retErr, networkBinding.Close())
		}
	}()
	opts := []firecracker.Opt{}
	if restoring {
		opts = append(opts, withSnapshotRestore(snapshotMemoryPath, snapshotStatePath))
		opts = append(opts, withJailedRestoreFiles(c.cfg.RootfsPath, scratchDiskPath, computerDiskPath, snapshotMemoryPath, snapshotStatePath))
		if len(readOnlyDrives) != 0 {
			opts = append(opts, withRestoreSealedDrives(sealedDriveChrootStrategy{
				kernelImagePath: c.cfg.KernelPath,
				drives:          readOnlyDrives,
			}))
		}
	}
	opts = append(opts, c.withTapOwner())
	opts = append(opts, c.withNetworkBinding(mode, owner, binding, &networkBinding))
	diskFiles, err := openRuntimeDiskFiles(scratchDiskPath, computerDiskPath)
	if err != nil {
		return nil, err
	}
	if retained != nil {
		retained.files = diskFiles
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, closeRuntimeDiskFiles(diskFiles))
		}
	}()
	// firecracker-go-sdk binds this context to the jailer/firecracker process.
	// Keep it separate from the startup request so prepared machines can outlive
	// a background warm command after boot succeeds.
	machineCtx, machineCancel := context.WithCancel(context.Background())
	phaseStarted = time.Now()
	sdkMachine, err := newSDKMachine(machineCtx, machineCfg, c.cfg.InitTimeout, opts...)
	reportPhase(recordPhase, vm.Phase{Name: "restore_create_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		machineCancel()
		return nil, fmt.Errorf("create Firecracker machine: %w", err)
	}
	var exportFailure *computerExportWatch
	if retained != nil && retained.device != nil {
		exportFailure = watchComputerExport(machineCtx, machineCancel, retained.device)
		defer func() {
			if retErr != nil {
				machineCancel()
				retErr = errors.Join(retErr, exportFailure.join())
			}
		}()
	}
	sdkMachine.Logger().Printf("starting Firecracker machine")
	phaseStarted = time.Now()
	if err := startMachineContext(ctx, sdkMachine, machineCtx, machineCancel); err != nil {
		reportPhase(recordPhase, vm.Phase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		stopErr := stopMachine(context.Background(), sdkMachine)
		return nil, errors.Join(fmt.Errorf("start Firecracker machine: %w", err), stopErr)
	}
	reportPhase(recordPhase, vm.Phase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	machineExit := watchMachineExit(sdkMachine)
	sdkMachine.Logger().Printf("Firecracker machine start returned")
	started := true
	defer func() {
		if !started {
			stopErr := stopGuestMachine(context.Background(), sdkMachine, machineExit)
			machineCancel()
			retErr = errors.Join(retErr, stopErr)
		}
	}()
	if restoring {
		phaseStarted = time.Now()
		if err := validateRestoredNetworkConfig(*restoreNetwork, snapshotNetworkConfig(c.cfg)); err != nil {
			reportPhase(recordPhase, vm.Phase{Name: "restore_validate_network", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			started = false
			return nil, err
		}
		reportPhase(recordPhase, vm.Phase{Name: "restore_validate_network", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
		phaseStarted = time.Now()
		if err := sdkMachine.ResumeVM(ctx); err != nil {
			reportPhase(recordPhase, vm.Phase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			started = false
			return nil, fmt.Errorf("resume restored Firecracker machine: %w", err)
		}
		reportPhase(recordPhase, vm.Phase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	}
	sdkMachine.Logger().Printf("waiting for guest health")
	phaseStarted = time.Now()
	err = c.waitForHealth(ctx, vsockHostPath, machineExit, sdkMachine.Logger().Printf)
	reportPhase(recordPhase, vm.Phase{Name: "restore_wait_guest_health", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		started = false
		return nil, err
	}
	sdkMachine.Logger().Printf("guest health ready")
	machine := &guestMachine{
		machine:         sdkMachine,
		machineCancel:   machineCancel,
		machineExit:     machineExit,
		computerExport:  exportFailure,
		cfg:             launchCfg,
		kernelArgs:      c.kernelArgsValue(),
		vmPlatform:      vmPlatform,
		cpuConfigDigest: cpuConfigDigest,
		vsockHostPath:   vsockHostPath,
		instanceDir:     instanceDir,
		jailRoot:        jailRoot,
		scratchDisk:     scratchDiskPath,
		diskFiles:       diskFiles,
		topology:        topology,
		readOnlyDrives:  append([]vm.ReadOnlyDrive(nil), readOnlyDrives...),
		owner:           owner,
		cleaner:         connectorCleaner{connector: c},
		networkBinding:  networkBinding,
	}
	machine.watchNetworkFailure()
	return machine, nil
}

func (c *Connector) boundMachineRuntime(vcpuCount int64) (vmplatform.Profile, string, string, error) {
	vmPlatform, err := c.hostRuntime.vmPlatform()
	if err != nil {
		return vmplatform.Profile{}, "", "", fmt.Errorf("resolve host runtime identity: %w", err)
	}
	cpuConfigDigest, err := c.hostRuntime.cpuConfigDigest(vcpuCount)
	if err != nil {
		return vmplatform.Profile{}, "", "", fmt.Errorf("resolve guest CPU configuration for %d vCPUs: %w", vcpuCount, err)
	}
	firecrackerPath, err := c.hostRuntime.firecrackerExecutable()
	if err != nil {
		return vmplatform.Profile{}, "", "", fmt.Errorf("resolve pinned Firecracker executable: %w", err)
	}
	return vmPlatform, cpuConfigDigest, firecrackerPath, nil
}

func startMachineContext(ctx context.Context, sdkMachine *firecracker.Machine, machineCtx context.Context, machineCancel context.CancelFunc) error {
	result := make(chan error, 1)
	go func() {
		result <- sdkMachine.Start(machineCtx)
	}()
	select {
	case err := <-result:
		if ctx.Err() != nil {
			machineCancel()
			return ctx.Err()
		}
		if err != nil {
			machineCancel()
		}
		return err
	case <-ctx.Done():
		machineCancel()
		// Startup handlers may still publish network resources. Join them before
		// the caller stops the VMM and cleans those resources. This wait is
		// cooperative: SDK handlers and the allocation flock can delay return.
		<-result
		return ctx.Err()
	}
}

func (c *Connector) createScratchDisk(ctx context.Context, scratchDiskPath string) error {
	file, err := os.OpenFile(scratchDiskPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create scratch disk: %w", err)
	}
	size := c.cfg.ScratchDiskMiB * 1024 * 1024
	truncateErr := file.Truncate(size)
	closeErr := file.Close()
	if truncateErr != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("size scratch disk: %w", truncateErr)
	}
	if closeErr != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("close scratch disk: %w", closeErr)
	}
	cmd := exec.CommandContext(
		ctx,
		c.cfg.MkfsExt4Path,
		"-F",
		"-q",
		"-m",
		"0",
		scratchDiskPath,
	)
	cmd.Env = []string{
		"LC_ALL=C.UTF-8",
		"LANG=C.UTF-8",
		"TZ=UTC",
		"MKE2FS_CONFIG=" + c.cfg.Mke2fsConfigPath,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("format scratch disk: %w: %s", err, strings.TrimSpace(string(output)))
	}
	floor := c.scratchUsableFloor()
	if floor > 0 {
		usable, err := ext4FreeBytes(scratchDiskPath)
		if err != nil {
			_ = os.Remove(scratchDiskPath)
			return fmt.Errorf("inspect scratch filesystem: %w", err)
		}
		if usable < floor {
			_ = os.Remove(scratchDiskPath)
			return fmt.Errorf(
				"scratch filesystem has %d usable bytes, build contract requires at least %d",
				usable,
				floor,
			)
		}
	}
	return nil
}

func (c *Connector) scratchUsableFloor() uint64 {
	return 0
}

func ext4FreeBytes(path string) (uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	superblock := make([]byte, ext4SuperblockBytes)
	if _, err := io.ReadFull(
		io.NewSectionReader(file, ext4SuperblockOffset, ext4SuperblockBytes),
		superblock,
	); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint16(superblock[56:58]) != ext4Magic {
		return 0, errors.New("scratch filesystem is not ext4")
	}
	logBlockSize := binary.LittleEndian.Uint32(superblock[24:28])
	if logBlockSize > 6 {
		return 0, fmt.Errorf("invalid ext4 block size shift %d", logBlockSize)
	}
	freeBlocks := uint64(binary.LittleEndian.Uint32(superblock[12:16])) |
		uint64(binary.LittleEndian.Uint32(superblock[0x158:0x15c]))<<32
	blockSize := uint64(1024) << logBlockSize
	if freeBlocks > ^uint64(0)/blockSize {
		return 0, errors.New("ext4 free byte count overflows")
	}
	return freeBlocks * blockSize, nil
}

func runtimeMachineConfiguration(descriptor VMRuntimeDescriptor, cfg Config) models.MachineConfiguration {
	return models.MachineConfiguration{
		VcpuCount:       firecracker.Int64(cfg.VCPUCount),
		MemSizeMib:      firecracker.Int64(cfg.MemoryMiB),
		Smt:             firecracker.Bool(descriptor.Machine.SMT),
		TrackDirtyPages: descriptor.Machine.TrackDirtyPages,
	}
}

func runtimeVsockDevice(descriptor VMRuntimeDescriptor, guestCID uint32) firecracker.VsockDevice {
	return firecracker.VsockDevice{
		ID: descriptor.Devices.Vsock.ID, Path: descriptor.Paths.VsockSocket, CID: guestCID,
	}
}

func reportPhase(record func(vm.Phase), phase vm.Phase) {
	if record == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	record(phase)
}

func allocateGuestCID() uint32 {
	return CanonicalVMRuntimeDescriptor().Devices.Vsock.GuestCIDStart - 1 + nextGuestCID.Add(1)
}
