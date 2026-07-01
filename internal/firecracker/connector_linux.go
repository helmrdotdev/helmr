//go:build linux

package firecracker

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
	"github.com/firecracker-microvm/firecracker-go-sdk/vsock"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/vm"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const defaultKernelArgs = "console=ttyS0 reboot=k panic=1 root=/dev/vda rootfstype=ext4 ro init=/init"
const stopTimeout = 10 * time.Second
const runtimeABI = "helmr.firecracker.snapshot.v0"
const apiSocketName = "api.sock"
const vsockSocketName = "vsock.sock"
const scratchDiskName = "scratch.ext4"
const maxGuestHealthResponseBytes = 4096

var nextGuestCID atomic.Uint32
var dialVsock = vsock.DialContext

type Connector struct {
	cfg Config
}

func NewConnector(cfg Config) (*Connector, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Connector{cfg: cfg}, nil
}

func (c *Connector) RuntimeCapabilities() (RuntimeCapabilities, error) {
	kernelDigest, err := digestFile(c.cfg.KernelPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest kernel: %w", err)
	}
	initramfsDigest, err := digestFile(c.cfg.InitramfsPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest initramfs: %w", err)
	}
	rootfsDigest, err := digestFile(c.cfg.RootfsPath)
	if err != nil {
		return RuntimeCapabilities{}, fmt.Errorf("digest guest rootfs: %w", err)
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
	})
	if err != nil {
		return RuntimeCapabilities{}, err
	}
	return RuntimeCapabilities{
		ID:              runtimeID,
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
		VCPUCount:       c.cfg.VCPUCount,
		MemoryMiB:       c.cfg.MemoryMiB,
	}, nil
}

func (c *Connector) Connect(ctx context.Context, request vm.ConnectRequest) (vm.Session, error) {
	return c.start(ctx, "", "", "", nil, request.Network, request.Topology, nil)
}

func (c *Connector) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if err := c.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	cfg, err := c.configForMaterializeRequest(request)
	if err != nil {
		return nil, err
	}
	child := *c
	child.cfg = cfg
	return child.start(ctx, "", "", "", nil, request.Network, request.Topology, nil)
}

func (c *Connector) validateMaterializeRequest(request vm.MaterializeRequest) error {
	if strings.TrimSpace(request.ImageFormat) != "oci-tar" {
		return fmt.Errorf("firecracker materialize image format %q is not supported", request.ImageFormat)
	}
	rootfsDigest, err := digestFile(c.cfg.RootfsPath)
	if err != nil {
		return fmt.Errorf("digest workspaceMount rootfs: %w", err)
	}
	if rootfsDigest != strings.TrimSpace(request.RootfsDigest) {
		return fmt.Errorf("workspaceMount rootfs digest %s does not match declared digest %s", rootfsDigest, request.RootfsDigest)
	}
	if strings.TrimSpace(request.ImageDigest) == "" {
		return errors.New("firecracker materialize image digest is required")
	}
	return nil
}

func (c *Connector) configForMaterializeRequest(request vm.MaterializeRequest) (Config, error) {
	cfg := c.cfg
	if request.Resources.MemoryMiB > 0 {
		if request.Resources.MemoryMiB > cfg.MemoryMiB {
			return Config{}, fmt.Errorf("materialize requested memory %d MiB exceeds worker VM memory capacity %d MiB", request.Resources.MemoryMiB, cfg.MemoryMiB)
		}
		cfg.MemoryMiB = request.Resources.MemoryMiB
	}
	if request.Resources.MilliCPU > 0 {
		requestedVCPUs := (request.Resources.MilliCPU + 999) / 1000
		if requestedVCPUs <= 0 {
			requestedVCPUs = 1
		}
		if requestedVCPUs > cfg.VCPUCount {
			return Config{}, fmt.Errorf("materialize requested cpu %d milliCPU exceeds worker VM vCPU capacity %d", request.Resources.MilliCPU, cfg.VCPUCount)
		}
		cfg.VCPUCount = requestedVCPUs
	}
	if request.Resources.DiskMiB > 0 {
		if request.Resources.DiskMiB > cfg.ScratchDiskMiB {
			return Config{}, fmt.Errorf("materialize requested disk %d MiB exceeds worker VM scratch disk capacity %d MiB", request.Resources.DiskMiB, cfg.ScratchDiskMiB)
		}
		cfg.ScratchDiskMiB = request.Resources.DiskMiB
	}
	return cfg, nil
}

func (c *Connector) Restore(ctx context.Context, request vm.RestoreRequest) (vm.Session, error) {
	if len(request.Memory) != 1 {
		return nil, fmt.Errorf("firecracker restore requires exactly one memory file, got %d", len(request.Memory))
	}
	if len(request.MemoryMediaTypes) != 1 {
		return nil, fmt.Errorf("firecracker restore requires exactly one memory media type, got %d", len(request.MemoryMediaTypes))
	}
	if strings.TrimSpace(request.VMState) == "" {
		return nil, errors.New("firecracker restore vm state path is required")
	}
	if request.VMStateMediaType != cas.CheckpointVMStateMediaType {
		return nil, fmt.Errorf("firecracker restore vm state media type %q is not supported", request.VMStateMediaType)
	}
	recordPhase := request.RecordPhase
	started := time.Now()
	manifest, restoreCfg, err := c.validateRestoreIdentity(request.ID, request.Manifest, request.Checkpoint, request.Topology)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_validate_identity", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.ScratchDisk) == "" {
		return nil, errors.New("firecracker restore scratch disk path is required")
	}
	if request.ScratchDiskMediaType != cas.CheckpointScratchDiskMediaType {
		return nil, fmt.Errorf("firecracker restore scratch disk media type %q is not supported", request.ScratchDiskMediaType)
	}
	if request.MemoryMediaTypes[0] != cas.CheckpointMemoryMediaType {
		return nil, fmt.Errorf("firecracker restore memory media type %q is not supported", request.MemoryMediaTypes[0])
	}
	expectedScratchSize := manifest.RecoveryPoint.Runtime.ScratchDiskMiB * 1024 * 1024
	expectedMemorySize := manifest.RecoveryPoint.Runtime.MemoryMiB * 1024 * 1024
	var rawScratch string
	var rawMemory string
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, request.ScratchDisk, filepackScratchRole, "scratch.ext4", expectedScratchSize, cas.CheckpointScratchDiskMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint scratch disk: %w", err)
		}
		rawScratch = path
		return nil
	})
	group.Go(func() error {
		path, phase, err := c.unpackRestoreArtifact(groupCtx, request.Memory[0], filepackMemoryRole, "memory.mem", expectedMemorySize, cas.CheckpointMemoryMediaType)
		recordRuntimePhase(recordPhase, phase)
		if err != nil {
			return fmt.Errorf("unpack checkpoint memory: %w", err)
		}
		rawMemory = path
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{rawScratch, rawMemory})
		return nil, err
	}
	cleanup := []string{rawScratch, rawMemory}
	child := *c
	child.cfg = restoreCfg
	session, err := child.start(ctx, rawMemory, request.VMState, rawScratch, &manifest.RuntimeState.Network, request.Network, request.Topology, recordPhase)
	if err != nil {
		removeFiles(cleanup)
		return nil, err
	}
	return restoreCleanupSession{CheckpointableSession: session, paths: cleanup}, nil
}

func (c *Connector) validateRestoreIdentity(checkpointID string, manifestBytes []byte, identity vm.CheckpointIdentity, topology vm.RuntimeTopology) (snapshotManifest, Config, error) {
	var manifest snapshotManifest
	if identity.RuntimeBackend != "firecracker" {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime backend %q is not supported", identity.RuntimeBackend)
	}
	if identity.RuntimeArch != runtime.GOARCH {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime arch %q does not match worker arch %q", identity.RuntimeArch, runtime.GOARCH)
	}
	if identity.RuntimeABI != runtimeABI {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime abi %q does not match worker abi %q", identity.RuntimeABI, runtimeABI)
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
	kernelDigest, err := digestFile(c.cfg.KernelPath)
	if err != nil {
		return manifest, Config{}, fmt.Errorf("digest guest kernel: %w", err)
	}
	if identity.KernelDigest != kernelDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint kernel digest %s does not match worker kernel digest %s", identity.KernelDigest, kernelDigest)
	}
	initramfsDigest, err := digestFile(c.cfg.InitramfsPath)
	if err != nil {
		return manifest, Config{}, fmt.Errorf("digest guest initramfs: %w", err)
	}
	if identity.InitramfsDigest != initramfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint initramfs digest %s does not match worker initramfs digest %s", identity.InitramfsDigest, initramfsDigest)
	}
	rootfsDigest, err := digestFile(c.cfg.RootfsPath)
	if err != nil {
		return manifest, Config{}, fmt.Errorf("digest guest rootfs: %w", err)
	}
	if identity.RootfsDigest != rootfsDigest {
		return manifest, Config{}, fmt.Errorf("checkpoint rootfs digest %s does not match worker rootfs digest %s", identity.RootfsDigest, rootfsDigest)
	}
	if identity.RuntimeConfigDigest != sha256sum.DigestBytes(manifestBytes) {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime config digest %s does not match checkpoint manifest digest %s", identity.RuntimeConfigDigest, sha256sum.DigestBytes(manifestBytes))
	}
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      c.cfg.CNIProfile,
	})
	if err != nil {
		return manifest, Config{}, err
	}
	if identity.RuntimeID != runtimeID {
		return manifest, Config{}, fmt.Errorf("checkpoint runtime id %s does not match worker runtime id %s", identity.RuntimeID, runtimeID)
	}
	restoreCfg, err := c.configForRestoreManifest(manifest)
	if err != nil {
		return manifest, Config{}, err
	}
	if err := validateRuntimeManifest(restoreCfg, manifest, runtimeID, kernelDigest, initramfsDigest, rootfsDigest, topology.Substrate); err != nil {
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

func (c *Connector) networkInterface(restoreNetwork *snapshotNetworkManifest) firecracker.NetworkInterface {
	cni := &firecracker.CNIConfiguration{
		NetworkName: c.cfg.CNINetworkName,
		ConfDir:     c.cfg.CNIConfDir,
		BinPath:     []string{c.cfg.CNIBinDir},
		CacheDir:    c.cfg.CNICacheDir,
		IfName:      c.cfg.CNIIfName,
		VMIfName:    c.cfg.CNIVMIfName,
	}
	if restoreNetwork != nil && restoreNetwork.GuestIPCIDR != "" {
		cni.Args = [][2]string{{"IP", restoreNetwork.GuestIPCIDR}}
	}
	return firecracker.NetworkInterface{CNIConfiguration: cni}
}

func (c *Connector) unpackRestoreArtifact(ctx context.Context, artifactPath string, role string, suffix string, expectedLogicalSize int64, mediaType string) (string, vm.RuntimePhase, error) {
	started := time.Now()
	phase := vm.RuntimePhase{
		Name:      "restore_unpack_" + strings.ReplaceAll(role, "-", "_") + "_filepack",
		Role:      role,
		MediaType: mediaType,
	}
	if role == filepackScratchRole {
		phase.Name = "restore_unpack_scratch_filepack"
	}
	if err := os.MkdirAll(c.cfg.StateDir, 0o700); err != nil {
		phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	file, err := os.CreateTemp(c.cfg.StateDir, "restore-*."+suffix)
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
	stats, err := unpackRuntimeFile(ctx, artifactPath, targetPath, role, expectedLogicalSize)
	phase.DurationMs = vm.RuntimeDurationMilliseconds(time.Since(started))
	if err == nil || stats.LogicalBytes != 0 || stats.EncodedChunks != 0 || stats.UnpackWrittenBytes != 0 {
		phase.Filepack = &stats
	}
	if err != nil {
		_ = os.Remove(targetPath)
		phase.ErrorClass = vm.RuntimeErrorClass(err)
		return "", phase, err
	}
	return targetPath, phase, nil
}

type restoreCleanupSession struct {
	vm.CheckpointableSession
	paths []string
}

func (s restoreCleanupSession) Close(ctx context.Context) error {
	err := s.CheckpointableSession.Close(ctx)
	removeFiles(s.paths)
	return err
}

func removeFiles(paths []string) {
	for _, path := range paths {
		_ = os.Remove(path)
	}
}

func (c *Connector) start(ctx context.Context, snapshotMemoryPath string, snapshotStatePath string, scratchDiskRestorePath string, restoreNetwork *snapshotNetworkManifest, network compute.NetworkPolicy, topology vm.RuntimeTopology, recordPhase func(vm.RuntimePhase)) (vm.CheckpointableSession, error) {
	instanceID := uuid.NewString()
	instanceDir := filepath.Join(c.cfg.StateDir, instanceID)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return nil, fmt.Errorf("create firecracker instance dir: %w", err)
	}
	cleanupInstanceDir := func() { _ = os.RemoveAll(instanceDir) }
	scratchDiskPath := filepath.Join(instanceDir, scratchDiskName)
	if strings.TrimSpace(scratchDiskRestorePath) != "" {
		scratchDiskPath = scratchDiskRestorePath
	} else if err := c.createScratchDisk(ctx, scratchDiskPath); err != nil {
		cleanupInstanceDir()
		return nil, err
	}
	phaseStarted := time.Now()
	if err := c.prepareScratchDiskForJailer(scratchDiskPath); err != nil {
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		cleanupInstanceDir()
		return nil, err
	}
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_prepare_scratch_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	substrateDiskPath := ""
	if topology.Substrate != nil {
		if err := validateRuntimeSubstrateTopology(topology.Substrate); err != nil {
			cleanupInstanceDir()
			return nil, err
		}
		substrateDiskPath = strings.TrimSpace(topology.Substrate.Path)
		phaseStarted = time.Now()
		if err := c.prepareSubstrateDiskForJailer(substrateDiskPath); err != nil {
			recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "prepare_substrate_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			cleanupInstanceDir()
			return nil, err
		}
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "prepare_substrate_for_jailer", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	}
	jailRoot := jailRootPath(c.cfg, instanceID)
	cleanup := func() {
		cleanupInstanceDir()
		_ = os.RemoveAll(filepath.Dir(jailRoot))
	}

	vsockHostPath := filepath.Join(jailRoot, vsockSocketName)
	guestCID := allocateGuestCID()
	machineCfg := firecracker.Config{
		VMID:            instanceID,
		SocketPath:      apiSocketName,
		LogLevel:        "Info",
		KernelImagePath: c.cfg.KernelPath,
		InitrdPath:      c.cfg.InitramfsPath,
		KernelArgs:      defaultKernelArgs,
		NetNS:           filepath.Join("/var/run/netns", instanceID),
		Seccomp: firecracker.SeccompConfig{
			Enabled: true,
		},
		JailerCfg: &firecracker.JailerConfig{
			UID:            firecracker.Int(c.cfg.JailerUID),
			GID:            firecracker.Int(c.cfg.JailerGID),
			ID:             instanceID,
			NumaNode:       firecracker.Int(c.cfg.JailerNumaNode),
			ExecFile:       c.cfg.FirecrackerPath,
			JailerBinary:   c.cfg.JailerPath,
			ChrootBaseDir:  c.cfg.JailerChrootBaseDir,
			ChrootStrategy: firecracker.NewNaiveChrootStrategy(c.cfg.KernelPath),
			CgroupVersion:  c.cfg.CgroupVersion,
			Stdin:          nil,
			Stdout:         os.Stderr,
			Stderr:         os.Stderr,
		},
		Drives: runtimeDrives(c.cfg.RootfsPath, scratchDiskPath, substrateDiskPath),
		VsockDevices: []firecracker.VsockDevice{{
			ID:   "guest-vsock",
			Path: vsockSocketName,
			CID:  guestCID,
		}},
		NetworkInterfaces: firecracker.NetworkInterfaces{c.networkInterface(restoreNetwork)},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  firecracker.Int64(c.cfg.VCPUCount),
			MemSizeMib: firecracker.Int64(c.cfg.MemoryMiB),
			Smt:        firecracker.Bool(false),
		},
	}
	opts := []firecracker.Opt{}
	restoring := snapshotMemoryPath != "" || snapshotStatePath != ""
	if restoring {
		opts = append(opts, withSnapshotRestore(snapshotMemoryPath, snapshotStatePath))
		opts = append(opts, withJailedRestoreFiles(c.cfg.RootfsPath, scratchDiskPath, substrateDiskPath, snapshotMemoryPath, snapshotStatePath))
	}
	opts = append(opts, c.withTapOwner())
	opts = append(opts, c.withNetworkPolicy(instanceID, network))
	// firecracker-go-sdk binds this context to the jailer/firecracker process.
	// Keep it separate from the startup request so prepared sessions can outlive
	// a background warm command after boot succeeds.
	machineCtx, machineCancel := context.WithCancel(context.Background())
	phaseStarted = time.Now()
	machine, err := firecracker.NewMachine(machineCtx, machineCfg, opts...)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_create_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		machineCancel()
		cleanup()
		return nil, fmt.Errorf("create firecracker machine: %w", err)
	}
	machine.Logger().Printf("starting firecracker machine")
	phaseStarted = time.Now()
	if err := startMachineContext(ctx, machine, machineCtx, machineCancel); err != nil {
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
		_ = stopMachine(context.Background(), machine)
		_ = c.cleanupNetworkPolicy(context.Background(), instanceID)
		cleanup()
		return nil, fmt.Errorf("start firecracker machine: %w", err)
	}
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_start_firecracker_machine", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	machineExit := watchMachineExit(machine)
	machine.Logger().Printf("firecracker machine start returned")
	started := true
	defer func() {
		if !started {
			_ = stopSessionMachine(context.Background(), machine, machineExit)
			machineCancel()
			_ = c.cleanupNetworkPolicy(context.Background(), instanceID)
			cleanup()
		}
	}()
	if restoring {
		phaseStarted = time.Now()
		if err := machine.ResumeVM(ctx); err != nil {
			recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
			started = false
			return nil, fmt.Errorf("resume restored firecracker machine: %w", err)
		}
		recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_resume_firecracker_snapshot", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted))})
	}
	machine.Logger().Printf("waiting for guest health")
	phaseStarted = time.Now()
	conn, err := c.connectReadyGuest(ctx, vsockHostPath, machineExit, machine.Logger().Printf)
	recordRuntimePhase(recordPhase, vm.RuntimePhase{Name: "restore_wait_guest_health", DurationMs: vm.RuntimeDurationMilliseconds(time.Since(phaseStarted)), ErrorClass: vm.RuntimeErrorClass(err)})
	if err != nil {
		started = false
		return nil, err
	}
	machine.Logger().Printf("guest health ready")
	return &guestSession{
		stream:        conn,
		machine:       machine,
		machineCancel: machineCancel,
		machineExit:   machineExit,
		cfg:           c.cfg,
		vsockHostPath: vsockHostPath,
		instanceDir:   instanceDir,
		jailRoot:      jailRoot,
		scratchDisk:   scratchDiskPath,
		topology:      topology,
		cleanup:       cleanup,
		networkPolicyCleanup: func() error {
			return c.cleanupNetworkPolicy(context.Background(), instanceID)
		},
	}, nil
}

func startMachineContext(ctx context.Context, machine *firecracker.Machine, machineCtx context.Context, machineCancel context.CancelFunc) error {
	result := make(chan error, 1)
	go func() {
		result <- machine.Start(machineCtx)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		machineCancel()
		return ctx.Err()
	}
}

func (c *Connector) connectReadyGuest(ctx context.Context, vsockHostPath string, machineExit *machineExit, logf func(string, ...interface{})) (io.ReadWriteCloser, error) {
	if err := c.waitForHealth(ctx, vsockHostPath, machineExit, logf); err != nil {
		return nil, err
	}
	return c.connectGuestPort(ctx, vsockHostPath, machineExit)
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
	cmd := exec.CommandContext(ctx, c.cfg.MkfsExt4Path, "-F", "-q", scratchDiskPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(scratchDiskPath)
		return fmt.Errorf("format scratch disk: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func runtimeDrives(rootfsPath string, scratchDiskPath string, substrateDiskPath string) []models.Drive {
	drives := []models.Drive{{
		DriveID:      firecracker.String("rootfs"),
		PathOnHost:   firecracker.String(rootfsPath),
		IsRootDevice: firecracker.Bool(true),
		IsReadOnly:   firecracker.Bool(true),
	}, {
		DriveID:      firecracker.String("scratch"),
		PathOnHost:   firecracker.String(scratchDiskPath),
		IsRootDevice: firecracker.Bool(false),
		IsReadOnly:   firecracker.Bool(false),
	}}
	if strings.TrimSpace(substrateDiskPath) != "" {
		drives = append(drives, models.Drive{
			DriveID:      firecracker.String("substrate"),
			PathOnHost:   firecracker.String(substrateDiskPath),
			IsRootDevice: firecracker.Bool(false),
			IsReadOnly:   firecracker.Bool(true),
		})
	}
	return drives
}

func (c *Connector) prepareScratchDiskForJailer(scratchDiskPath string) error {
	if err := os.Chown(scratchDiskPath, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
		return fmt.Errorf("chown scratch disk for jailer: %w", err)
	}
	if err := os.Chmod(scratchDiskPath, 0o600); err != nil {
		return fmt.Errorf("chmod scratch disk for jailer: %w", err)
	}
	return nil
}

func (c *Connector) prepareSubstrateDiskForJailer(substrateDiskPath string) error {
	if err := os.Chown(substrateDiskPath, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
		return fmt.Errorf("chown substrate disk for jailer: %w", err)
	}
	if err := os.Chmod(substrateDiskPath, 0o440); err != nil {
		return fmt.Errorf("chmod substrate disk for jailer: %w", err)
	}
	return nil
}

func (c *Connector) withTapOwner() firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.SetupNetworkHandlerName, firecracker.Handler{
			Name: "helmr.SetTapOwner",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				for _, iface := range machine.Cfg.NetworkInterfaces {
					if iface.StaticConfiguration == nil || iface.StaticConfiguration.HostDevName == "" {
						continue
					}
					if err := setTapOwner(machine.Cfg.NetNS, iface.StaticConfiguration.HostDevName, c.cfg.JailerUID, c.cfg.JailerGID); err != nil {
						return err
					}
				}
				return nil
			},
		})
	}
}

func setTapOwner(netNSPath string, tapName string, uid int, gid int) error {
	if strings.TrimSpace(netNSPath) == "" {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	}
	netNS, err := ns.GetNS(netNSPath)
	if err != nil {
		return fmt.Errorf("open network namespace %q: %w", netNSPath, err)
	}
	defer netNS.Close()
	return netNS.Do(func(ns.NetNS) error {
		return setTapOwnerInCurrentNetNS(tapName, uid, gid)
	})
}

func setTapOwnerInCurrentNetNS(tapName string, uid int, gid int) error {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/net/tun: %w", err)
	}
	defer unix.Close(fd)

	ifr, err := unix.NewIfreq(tapName)
	if err != nil {
		return fmt.Errorf("build tap ifreq %q: %w", tapName, err)
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return fmt.Errorf("open tap device %q: %w", tapName, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETOWNER, uid); err != nil {
		return fmt.Errorf("set tap %q owner uid %d: %w", tapName, uid, err)
	}
	if err := unix.IoctlSetInt(fd, unix.TUNSETGROUP, gid); err != nil {
		return fmt.Errorf("set tap %q owner gid %d: %w", tapName, gid, err)
	}
	return nil
}

func (c *Connector) waitForHealth(ctx context.Context, vsockPath string, machineExit *machineExit, logf func(string, ...interface{})) error {
	healthCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	stats := newHealthProbeStats()
	for {
		if err, ok := machineExit.Err(); ok {
			stats.machineExited = true
			stats.lastErr = err
			result := stats.failureError("firecracker machine exited during guest health wait", err)
			stats.log(logf, "failed")
			return result
		}
		stats.attempts++
		attemptStarted := time.Now()
		attemptCtx, cancelAttempt := healthAttemptContext(healthCtx, c.cfg.HealthAttemptTimeout)
		conn, err := dialVsock(attemptCtx, vsockPath, c.cfg.HealthPort)
		if err != nil {
			cancelAttempt()
			stats.recordError("dial", err)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), err, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if deadline, ok := attemptCtx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		response, readErr := readHealth(conn)
		closeErr := conn.Close()
		cancelAttempt()
		if readErr != nil {
			stats.recordError(healthProbeErrorBucket(readErr), readErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			if healthCtx.Err() != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, healthCtx.Err(), readErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			if err := sleepHealthRetry(healthCtx); err != nil {
				result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
				stats.log(logf, "failed")
				return result
			}
			continue
		}
		if closeErr != nil {
			stats.recordError("close", closeErr)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError("close guest health connection", closeErr)
			stats.log(logf, "failed")
			return result
		}
		if response.Status == "ok" && response.Component == "guestd" {
			stats.log(logf, "ready")
			return nil
		}
		if response.Status != "starting" {
			stats.recordStatus(response.Status)
			stats.logFailedAttempt(logf, time.Since(attemptStarted))
			result := stats.failureError(fmt.Sprintf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message), nil)
			stats.log(logf, "failed")
			return result
		}
		stats.recordStarting(response)
		if err := sleepHealthRetry(healthCtx); err != nil {
			result := stats.timeoutError(c.cfg.HealthTimeout, err, stats.lastErr, machineExit)
			stats.log(logf, "failed")
			return result
		}
	}
}

func healthAttemptContext(ctx context.Context, attemptTimeout time.Duration) (context.Context, context.CancelFunc) {
	if attemptTimeout <= 0 {
		attemptTimeout = DefaultHealthAttemptTimeout
	}
	return context.WithTimeout(ctx, attemptTimeout)
}

func (c *Connector) connectGuestPort(ctx context.Context, vsockPath string, machineExit *machineExit) (io.ReadWriteCloser, error) {
	connectCtx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	if machineExit != nil {
		go func() {
			select {
			case <-machineExit.done:
				cancel()
			case <-connectCtx.Done():
			}
		}()
	}
	var lastErr error
	for {
		if err, ok := machineExit.Err(); ok {
			return nil, fmt.Errorf("firecracker machine exited before guest port %d connection: %w", c.cfg.GuestPort, err)
		}
		conn, err := dialVsock(connectCtx, vsockPath, c.cfg.GuestPort)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if connectCtx.Err() != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("firecracker machine exited before guest port %d connection: %w", c.cfg.GuestPort, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", c.cfg.GuestPort, c.cfg.HealthTimeout, errors.Join(connectCtx.Err(), lastErr))
		}
		if err := sleepHealthRetry(connectCtx); err != nil {
			if exitErr, ok := machineExit.Err(); ok {
				return nil, fmt.Errorf("firecracker machine exited before guest port %d connection: %w", c.cfg.GuestPort, exitErr)
			}
			return nil, fmt.Errorf("guest port %d connection timed out after %s: %w", c.cfg.GuestPort, c.cfg.HealthTimeout, errors.Join(err, lastErr))
		}
	}
}

func sleepHealthRetry(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type healthResponse struct {
	Status    string `json:"status"`
	Component string `json:"component"`
	Message   string `json:"message,omitempty"`
}

func readHealth(conn io.ReadWriter) (healthResponse, error) {
	req, err := http.NewRequest(http.MethodGet, "http://guestd/", nil)
	if err != nil {
		return healthResponse{}, fmt.Errorf("build guest health request: %w", err)
	}
	req.Close = true
	if err := req.Write(conn); err != nil {
		return healthResponse{}, fmt.Errorf("write guest health request: %w", err)
	}
	httpResponse, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	defer httpResponse.Body.Close()
	body, err := io.ReadAll(io.LimitReader(httpResponse.Body, maxGuestHealthResponseBytes+1))
	if err != nil {
		return healthResponse{}, fmt.Errorf("read guest health response: %w", err)
	}
	if len(body) > maxGuestHealthResponseBytes {
		return healthResponse{}, fmt.Errorf("read guest health response: body exceeds %d bytes", maxGuestHealthResponseBytes)
	}
	if httpResponse.StatusCode != http.StatusOK {
		return healthResponse{}, fmt.Errorf("guest health returned HTTP %s: %s", httpResponse.Status, strings.TrimSpace(string(body)))
	}
	var response healthResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return healthResponse{}, fmt.Errorf("decode guest health response: %w", err)
	}
	return response, nil
}

type healthProbeStats struct {
	started           time.Time
	attempts          int
	dialErrors        int
	writeErrors       int
	readErrors        int
	statusErrors      int
	decodeErrors      int
	closeErrors       int
	startingResponses int
	machineExited     bool
	lastBucket        string
	lastStatus        string
	lastErr           error
}

func newHealthProbeStats() *healthProbeStats {
	return &healthProbeStats{started: time.Now()}
}

func (s *healthProbeStats) elapsed() time.Duration {
	if s == nil || s.started.IsZero() {
		return 0
	}
	return time.Since(s.started)
}

func (s *healthProbeStats) recordError(bucket string, err error) {
	if s == nil {
		return
	}
	if strings.TrimSpace(bucket) == "" {
		bucket = "unknown"
	}
	switch bucket {
	case "dial":
		s.dialErrors++
	case "write":
		s.writeErrors++
	case "read":
		s.readErrors++
	case "status":
		s.statusErrors++
	case "decode":
		s.decodeErrors++
	case "close":
		s.closeErrors++
	}
	s.lastBucket = bucket
	s.lastErr = err
}

func (s *healthProbeStats) recordStarting(response healthResponse) {
	if s == nil {
		return
	}
	s.startingResponses++
	s.lastBucket = "starting"
	s.lastStatus = response.Status
	s.lastErr = fmt.Errorf("guest health status=%q component=%q message=%q", response.Status, response.Component, response.Message)
}

func (s *healthProbeStats) recordStatus(status string) {
	if s == nil {
		return
	}
	s.statusErrors++
	s.lastBucket = "status"
	s.lastStatus = status
	s.lastErr = fmt.Errorf("guest health status=%q", status)
}

func (s *healthProbeStats) timeoutError(timeout time.Duration, err error, lastErr error, machineExit *machineExit) error {
	if s == nil {
		return fmt.Errorf("guest health probe timed out after %s: %w", timeout, errors.Join(err, lastErr))
	}
	if exitErr, ok := machineExit.Err(); ok {
		s.machineExited = true
		lastErr = errors.Join(lastErr, fmt.Errorf("firecracker machine exited: %w", exitErr))
	}
	return fmt.Errorf("guest health probe timed out after %s (%s): %w", timeout, s.summary(), errors.Join(err, lastErr))
}

func (s *healthProbeStats) failureError(message string, err error) error {
	if s == nil {
		if err == nil {
			return errors.New(message)
		}
		return fmt.Errorf("%s: %w", message, err)
	}
	if err == nil {
		return fmt.Errorf("%s (%s)", message, s.summary())
	}
	return fmt.Errorf("%s (%s): %w", message, s.summary(), err)
}

func (s *healthProbeStats) log(logf func(string, ...interface{}), status string) {
	if s == nil || logf == nil {
		return
	}
	logf("guest health probe %s %s", status, s.summary())
}

func (s *healthProbeStats) logFailedAttempt(logf func(string, ...interface{}), duration time.Duration) {
	if s == nil || logf == nil {
		return
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	logf("guest health probe attempt %s attempt=%d duration_ms=%d bucket=%q error=%q",
		"failed",
		s.attempts,
		vm.RuntimeDurationMilliseconds(duration),
		s.lastBucket,
		lastErr,
	)
}

func (s *healthProbeStats) summary() string {
	if s == nil {
		return ""
	}
	lastErr := ""
	if s.lastErr != nil {
		lastErr = strings.ReplaceAll(s.lastErr.Error(), "\n", " ")
	}
	return fmt.Sprintf("attempts=%d elapsed_ms=%d dial_errors=%d write_errors=%d read_errors=%d status_errors=%d decode_errors=%d close_errors=%d starting_responses=%d machine_exited=%t last_bucket=%q last_status=%q last_error=%q",
		s.attempts,
		vm.RuntimeDurationMilliseconds(s.elapsed()),
		s.dialErrors,
		s.writeErrors,
		s.readErrors,
		s.statusErrors,
		s.decodeErrors,
		s.closeErrors,
		s.startingResponses,
		s.machineExited,
		s.lastBucket,
		s.lastStatus,
		lastErr,
	)
}

func healthProbeErrorBucket(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "write guest health request"):
		return "write"
	case strings.Contains(message, "decode guest health response"):
		return "decode"
	case strings.Contains(message, "guest health returned http"):
		return "status"
	case strings.Contains(message, "read guest health response"):
		return "read"
	default:
		return "unknown"
	}
}

type guestSession struct {
	stream               io.ReadWriteCloser
	machine              *firecracker.Machine
	machineCancel        context.CancelFunc
	machineExit          *machineExit
	cfg                  Config
	vsockHostPath        string
	instanceDir          string
	jailRoot             string
	scratchDisk          string
	topology             vm.RuntimeTopology
	cleanup              func()
	networkPolicyCleanup func() error
	paused               atomic.Bool
	once                 sync.Once
	err                  error
}

func (s *guestSession) Stream() io.ReadWriteCloser {
	return s.stream
}

func (s *guestSession) OpenStream(ctx context.Context) (io.ReadWriteCloser, error) {
	return (&Connector{cfg: s.cfg}).connectGuestPort(ctx, s.vsockHostPath, s.machineExit)
}

func (s *guestSession) Wait(ctx context.Context) error {
	if s.machineExit == nil {
		return errors.New("firecracker session exit watcher is not configured")
	}
	return s.machineExit.Wait(ctx)
}

func (s *guestSession) Close(ctx context.Context) error {
	s.once.Do(func() {
		// Keep the machine context alive until StopVMM/Wait lets the SDK finish CNI cleanup.
		stopErr := stopSessionMachine(ctx, s.machine, s.machineExit)
		if s.machineCancel != nil {
			s.machineCancel()
		}
		streamErr := closeGuestStream(ctx, s.stream)
		if errors.Is(streamErr, net.ErrClosed) || errors.Is(streamErr, os.ErrClosed) {
			streamErr = nil
		}
		var networkPolicyErr error
		if s.networkPolicyCleanup != nil {
			networkPolicyErr = s.networkPolicyCleanup()
		}
		cleanupGuestSessionResources(s.cleanup)
		s.err = errors.Join(streamErr, networkPolicyErr, stopErr)
	})
	return s.err
}

func cleanupGuestSessionResources(cleanup func()) {
	if cleanup == nil {
		return
	}
	cleanup()
}

func closeGuestStream(ctx context.Context, stream io.Closer) error {
	ctx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- stream.Close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("close guest stream: %w", ctx.Err())
	}
}

func (s *guestSession) CreateSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	checkpointID := safeSnapshotID(request.ID)
	memName := checkpointID + ".mem"
	stateName := checkpointID + ".vmstate"
	memPath := filepath.Join(s.jailRoot, memName)
	statePath := filepath.Join(s.jailRoot, stateName)
	var phases []vm.RuntimePhase
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, vm.RuntimePhase{Name: name, DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	if err := s.machine.PauseVM(ctx); err != nil {
		return vm.SnapshotArtifact{}, fmt.Errorf("pause firecracker vm: %w", err)
	}
	recordPhase("firecracker_pause_vm", started)
	s.paused.Store(true)
	started = time.Now()
	if err := s.machine.CreateSnapshot(ctx, path.Join("/", memName), path.Join("/", stateName)); err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("create firecracker snapshot: %w", err)
	}
	recordPhase("firecracker_create_snapshot", started)
	cleanupRawSnapshot := true
	defer func() {
		if cleanupRawSnapshot {
			_ = os.Remove(memPath)
			_ = os.Remove(statePath)
		}
	}()
	started = time.Now()
	kernelDigest, err := digestFile(s.cfg.KernelPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest kernel: %w", err)
	}
	recordPhase("digest_kernel", started)
	started = time.Now()
	initramfsDigest, err := digestFile(s.cfg.InitramfsPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest initramfs: %w", err)
	}
	recordPhase("digest_initramfs", started)
	started = time.Now()
	rootfsDigest, err := digestFile(s.cfg.RootfsPath)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, fmt.Errorf("digest guest rootfs: %w", err)
	}
	recordPhase("digest_rootfs", started)
	runtimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            runtime.GOARCH,
		ABI:             runtimeABI,
		KernelDigest:    kernelDigest,
		InitramfsDigest: initramfsDigest,
		RootfsDigest:    rootfsDigest,
		CNIProfile:      s.cfg.CNIProfile,
	})
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	started = time.Now()
	configDigest, manifest, err := snapshotRuntimeConfig(s.cfg, s.machine, checkpointID, runtimeID, kernelDigest, initramfsDigest, rootfsDigest, s.topology)
	if err != nil {
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	recordPhase("runtime_config_digest", started)
	var scratchFile vm.SnapshotFile
	var memoryFile vm.SnapshotFile
	var scratchPhase vm.RuntimePhase
	var memoryPhase vm.RuntimePhase
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, s.scratchDisk, filepackScratchRole, checkpointID+".scratch.filepack", cas.CheckpointScratchDiskMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint scratch disk: %w", err)
		}
		scratchFile = file
		scratchPhase = phase
		return nil
	})
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, memPath, filepackMemoryRole, checkpointID+".memory.filepack", cas.CheckpointMemoryMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint memory: %w", err)
		}
		memoryFile = file
		memoryPhase = phase
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{scratchFile.Path, memoryFile.Path})
		_ = s.Resume(context.Background())
		return vm.SnapshotArtifact{}, err
	}
	phases = append(phases, scratchPhase, memoryPhase)
	_ = os.Remove(memPath)
	cleanupRawSnapshot = false
	return vm.SnapshotArtifact{
		RuntimeBackend:      "firecracker",
		RuntimeArch:         runtime.GOARCH,
		RuntimeABI:          runtimeABI,
		RuntimeID:           runtimeID,
		KernelDigest:        kernelDigest,
		InitramfsDigest:     initramfsDigest,
		RootfsDigest:        rootfsDigest,
		RuntimeConfigDigest: configDigest,
		Substrate:           cloneRuntimeSubstrate(s.topology.Substrate),
		VMState:             vm.SnapshotFile{Path: statePath, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk:         scratchFile,
		Memory:              []vm.SnapshotFile{memoryFile},
		Manifest:            manifest,
		Phases:              phases,
	}, nil
}

func (s *guestSession) packSnapshotRuntimeFile(ctx context.Context, sourcePath string, role string, name string, mediaType string) (vm.SnapshotFile, vm.RuntimePhase, error) {
	targetPath := filepath.Join(filepath.Dir(s.scratchDisk), name)
	started := time.Now()
	stats, err := packRuntimeFile(ctx, sourcePath, targetPath, role)
	if err != nil {
		return vm.SnapshotFile{}, vm.RuntimePhase{}, err
	}
	phaseName := "pack_" + strings.ReplaceAll(role, "-", "_") + "_filepack"
	if role == filepackScratchRole {
		phaseName = "pack_scratch_filepack"
	}
	return vm.SnapshotFile{Path: targetPath, MediaType: mediaType, Filepack: &stats}, vm.RuntimePhase{
		Name:       phaseName,
		DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
		Role:       role,
		MediaType:  mediaType,
		Filepack:   &stats,
	}, nil
}

func (s *guestSession) Resume(ctx context.Context) error {
	if !s.paused.Load() {
		return nil
	}
	if err := s.machine.ResumeVM(ctx); err != nil {
		return fmt.Errorf("resume firecracker vm: %w", err)
	}
	s.paused.Store(false)
	return nil
}

func recordRuntimePhase(record func(vm.RuntimePhase), phase vm.RuntimePhase) {
	if record == nil || strings.TrimSpace(phase.Name) == "" {
		return
	}
	record(phase)
}

func stopMachine(ctx context.Context, machine *firecracker.Machine) error {
	pid, pidErr := machine.PID()
	stopErr := machine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := machine.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill firecracker process %d: %w", pid, err))
		} else {
			killWaitCtx, killCancel := context.WithTimeout(context.Background(), stopTimeout)
			waitErr = machine.Wait(killWaitCtx)
			killCancel()
			waitErr = ignoreStopSignalError(waitErr, syscall.SIGKILL)
		}
	}
	return errors.Join(stopErr, ignoreExpectedStopErrors(waitErr))
}

type machineExit struct {
	done chan struct{}
	err  error
}

func watchMachineExit(machine *firecracker.Machine) *machineExit {
	exit := &machineExit{done: make(chan struct{})}
	go func() {
		exit.err = machine.Wait(context.Background())
		close(exit.done)
	}()
	return exit
}

func (e *machineExit) Wait(ctx context.Context) error {
	if e == nil {
		return errors.New("firecracker machine exit watcher is not configured")
	}
	select {
	case <-e.done:
		return e.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *machineExit) Err() (error, bool) {
	if e == nil {
		return nil, false
	}
	select {
	case <-e.done:
		return e.err, true
	default:
		return nil, false
	}
}

func stopSessionMachine(ctx context.Context, machine *firecracker.Machine, exit *machineExit) error {
	pid, pidErr := machine.PID()
	stopErr := machine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := exit.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill firecracker process %d: %w", pid, err))
		} else {
			killWaitCtx, killCancel := context.WithTimeout(context.Background(), stopTimeout)
			waitErr = exit.Wait(killWaitCtx)
			killCancel()
			waitErr = ignoreStopSignalError(waitErr, syscall.SIGKILL)
		}
	}
	return errors.Join(stopErr, ignoreExpectedStopErrors(waitErr))
}

func closeContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func ignoreExpectedStopErrors(err error) error {
	if err == nil {
		return nil
	}
	type wrappedErrors interface {
		WrappedErrors() []error
	}
	var wrapped wrappedErrors
	if errors.As(err, &wrapped) {
		var out error
		for _, nested := range wrapped.WrappedErrors() {
			out = errors.Join(out, ignoreExpectedStopErrors(nested))
		}
		return out
	}
	if ignoreStopSignalError(err, syscall.SIGTERM) == nil {
		return nil
	}
	return err
}

func ignoreStopSignalError(err error, signal syscall.Signal) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ProcessState != nil {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() && status.Signal() == signal {
			return nil
		}
	}
	return err
}

func safeSnapshotID(id string) string {
	if id == "" {
		return uuid.NewString()
	}
	out := make([]byte, 0, len(id))
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out = append(out, byte(r))
		}
	}
	if len(out) == 0 {
		return uuid.NewString()
	}
	return string(out)
}

func digestFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return sha256sum.DigestHash(hash), nil
}

type snapshotManifest struct {
	RecoveryPoint snapshotRecoveryPointManifest `json:"recovery_point"`
	RuntimeState  snapshotRuntimeStateManifest  `json:"runtime_state"`
}

type snapshotRecoveryPointManifest struct {
	ID      string                  `json:"id"`
	Runtime snapshotRuntimeManifest `json:"runtime"`
}

type snapshotRuntimeManifest struct {
	Backend         string                          `json:"backend"`
	ID              string                          `json:"id"`
	Arch            string                          `json:"arch"`
	ABI             string                          `json:"abi"`
	VCPUCount       int64                           `json:"vcpu_count"`
	MemoryMiB       int64                           `json:"memory_mib"`
	ScratchDiskMiB  int64                           `json:"scratch_disk_mib"`
	KernelArgs      string                          `json:"kernel_args"`
	KernelDigest    string                          `json:"kernel_digest"`
	InitramfsDigest string                          `json:"initramfs_digest"`
	RootfsDigest    string                          `json:"rootfs_digest"`
	Substrate       *snapshotRuntimeSubstrate       `json:"substrate,omitempty"`
	GuestPort       uint32                          `json:"guest_port"`
	HealthPort      uint32                          `json:"health_port"`
	Network         snapshotNetworkIdentityManifest `json:"network"`
}

type snapshotRuntimeSubstrate struct {
	Digest     string `json:"digest"`
	Format     string `json:"format"`
	BuilderABI string `json:"builder_abi"`
	LayoutABI  string `json:"layout_abi"`
}

type snapshotRuntimeStateManifest struct {
	Network snapshotNetworkManifest `json:"network"`
}

type snapshotNetworkIdentityManifest struct {
	Mode        string `json:"mode"`
	Profile     string `json:"profile"`
	NetworkName string `json:"network_name"`
	IfName      string `json:"if_name"`
	VMIfName    string `json:"vm_if_name"`
}

type snapshotNetworkManifest struct {
	Mode        string `json:"mode"`
	Profile     string `json:"profile"`
	NetworkName string `json:"network_name"`
	IfName      string `json:"if_name"`
	VMIfName    string `json:"vm_if_name"`
	GuestIPCIDR string `json:"guest_ip_cidr,omitempty"`
}

func snapshotSubstrateManifest(substrate *vm.RuntimeSubstrate) (*snapshotRuntimeSubstrate, error) {
	if substrate == nil {
		return nil, nil
	}
	if err := validateRuntimeSubstrateTopology(substrate); err != nil {
		return nil, err
	}
	return &snapshotRuntimeSubstrate{
		Digest:     strings.TrimSpace(substrate.Digest),
		Format:     strings.TrimSpace(substrate.Format),
		BuilderABI: strings.TrimSpace(substrate.BuilderABI),
		LayoutABI:  strings.TrimSpace(substrate.LayoutABI),
	}, nil
}

func cloneRuntimeSubstrate(substrate *vm.RuntimeSubstrate) *vm.RuntimeSubstrate {
	if substrate == nil {
		return nil
	}
	clone := *substrate
	return &clone
}

func validateRuntimeSubstrateTopology(substrate *vm.RuntimeSubstrate) error {
	if substrate == nil {
		return nil
	}
	if strings.TrimSpace(substrate.Path) == "" {
		return errors.New("runtime substrate path is required")
	}
	if strings.TrimSpace(substrate.Digest) == "" {
		return errors.New("runtime substrate digest is required")
	}
	if strings.TrimSpace(substrate.Format) != "ext4" {
		return fmt.Errorf("runtime substrate format %q is not supported", substrate.Format)
	}
	if strings.TrimSpace(substrate.BuilderABI) == "" {
		return errors.New("runtime substrate builder abi is required")
	}
	if strings.TrimSpace(substrate.LayoutABI) == "" {
		return errors.New("runtime substrate layout abi is required")
	}
	return nil
}

func snapshotRuntimeConfig(cfg Config, machine *firecracker.Machine, checkpointID string, runtimeID string, kernelDigest string, initramfsDigest string, rootfsDigest string, topology vm.RuntimeTopology) (string, []byte, error) {
	network := snapshotNetworkConfig(cfg, machine)
	if network.GuestIPCIDR == "" {
		return "", nil, errors.New("firecracker CNI guest IP is required for checkpoint restore")
	}
	substrate, err := snapshotSubstrateManifest(topology.Substrate)
	if err != nil {
		return "", nil, err
	}
	manifest, err := json.Marshal(snapshotManifest{
		RecoveryPoint: snapshotRecoveryPointManifest{
			ID: checkpointID,
			Runtime: snapshotRuntimeManifest{
				Backend:         "firecracker",
				ID:              runtimeID,
				Arch:            runtime.GOARCH,
				ABI:             runtimeABI,
				VCPUCount:       cfg.VCPUCount,
				MemoryMiB:       cfg.MemoryMiB,
				ScratchDiskMiB:  cfg.ScratchDiskMiB,
				KernelArgs:      defaultKernelArgs,
				KernelDigest:    kernelDigest,
				InitramfsDigest: initramfsDigest,
				RootfsDigest:    rootfsDigest,
				Substrate:       substrate,
				GuestPort:       cfg.GuestPort,
				HealthPort:      cfg.HealthPort,
				Network: snapshotNetworkIdentityManifest{
					Mode:        network.Mode,
					Profile:     network.Profile,
					NetworkName: network.NetworkName,
					IfName:      network.IfName,
					VMIfName:    network.VMIfName,
				},
			},
		},
		RuntimeState: snapshotRuntimeStateManifest{
			Network: network,
		},
	})
	if err != nil {
		return "", nil, fmt.Errorf("encode firecracker snapshot manifest: %w", err)
	}
	return sha256sum.DigestBytes(manifest), manifest, nil
}

func snapshotNetworkConfig(cfg Config, machine *firecracker.Machine) snapshotNetworkManifest {
	network := snapshotNetworkManifest{
		Mode:        "cni",
		Profile:     cfg.CNIProfile,
		NetworkName: cfg.CNINetworkName,
		IfName:      cfg.CNIIfName,
		VMIfName:    cfg.CNIVMIfName,
	}
	if machine == nil || len(machine.Cfg.NetworkInterfaces) == 0 {
		return network
	}
	static := machine.Cfg.NetworkInterfaces[0].StaticConfiguration
	if static == nil || static.IPConfiguration == nil {
		return network
	}
	network.GuestIPCIDR = static.IPConfiguration.IPAddr.String()
	return network
}

func validateRuntimeManifest(cfg Config, manifest snapshotManifest, runtimeID string, kernelDigest string, initramfsDigest string, rootfsDigest string, expectedSubstrate *vm.RuntimeSubstrate) error {
	runtimeManifest := manifest.RecoveryPoint.Runtime
	if runtimeManifest.Backend != "firecracker" {
		return fmt.Errorf("checkpoint manifest runtime backend %q is not supported", runtimeManifest.Backend)
	}
	if runtimeManifest.Arch != runtime.GOARCH {
		return fmt.Errorf("checkpoint manifest runtime arch %q does not match worker arch %q", runtimeManifest.Arch, runtime.GOARCH)
	}
	if runtimeManifest.ABI != runtimeABI {
		return fmt.Errorf("checkpoint manifest runtime abi %q does not match worker abi %q", runtimeManifest.ABI, runtimeABI)
	}
	if runtimeManifest.ID == "" {
		return errors.New("checkpoint manifest runtime id is required")
	}
	if runtimeManifest.ID != runtimeID {
		return fmt.Errorf("checkpoint manifest runtime id %s does not match worker runtime id %s", runtimeManifest.ID, runtimeID)
	}
	if runtimeManifest.KernelDigest != kernelDigest {
		return fmt.Errorf("checkpoint manifest kernel digest %s does not match worker kernel digest %s", runtimeManifest.KernelDigest, kernelDigest)
	}
	if runtimeManifest.InitramfsDigest != initramfsDigest {
		return fmt.Errorf("checkpoint manifest initramfs digest %s does not match worker initramfs digest %s", runtimeManifest.InitramfsDigest, initramfsDigest)
	}
	if runtimeManifest.RootfsDigest != rootfsDigest {
		return fmt.Errorf("checkpoint manifest rootfs digest %s does not match worker rootfs digest %s", runtimeManifest.RootfsDigest, rootfsDigest)
	}
	if err := validateRuntimeSubstrateManifest(runtimeManifest.Substrate, expectedSubstrate); err != nil {
		return err
	}
	if runtimeManifest.VCPUCount != cfg.VCPUCount || runtimeManifest.MemoryMiB != cfg.MemoryMiB {
		return fmt.Errorf("checkpoint manifest machine shape vcpu=%d memory=%d does not match worker vcpu=%d memory=%d", runtimeManifest.VCPUCount, runtimeManifest.MemoryMiB, cfg.VCPUCount, cfg.MemoryMiB)
	}
	if runtimeManifest.ScratchDiskMiB != cfg.ScratchDiskMiB {
		return fmt.Errorf("checkpoint manifest scratch disk size %d MiB does not match worker scratch disk size %d MiB", runtimeManifest.ScratchDiskMiB, cfg.ScratchDiskMiB)
	}
	if runtimeManifest.KernelArgs != defaultKernelArgs || runtimeManifest.GuestPort != cfg.GuestPort || runtimeManifest.HealthPort != cfg.HealthPort {
		return errors.New("checkpoint manifest runtime ports or kernel args do not match worker runtime")
	}
	networkIdentity := runtimeManifest.Network
	if networkIdentity.Mode != "cni" {
		return fmt.Errorf("checkpoint manifest network mode %q is not supported", networkIdentity.Mode)
	}
	if networkIdentity.Profile != cfg.CNIProfile || networkIdentity.NetworkName != cfg.CNINetworkName || networkIdentity.IfName != cfg.CNIIfName || networkIdentity.VMIfName != cfg.CNIVMIfName {
		return errors.New("checkpoint manifest CNI configuration does not match worker CNI configuration")
	}
	network := manifest.RuntimeState.Network
	if network.Mode != "cni" {
		return fmt.Errorf("checkpoint manifest network mode %q is not supported", network.Mode)
	}
	if network.Profile != cfg.CNIProfile || network.NetworkName != cfg.CNINetworkName || network.IfName != cfg.CNIIfName || network.VMIfName != cfg.CNIVMIfName {
		return errors.New("checkpoint manifest CNI configuration does not match worker CNI configuration")
	}
	if network.GuestIPCIDR == "" {
		return errors.New("checkpoint manifest guest_ip_cidr is required for CNI restore")
	}
	return nil
}

func validateRuntimeSubstrateManifest(manifest *snapshotRuntimeSubstrate, expected *vm.RuntimeSubstrate) error {
	switch {
	case manifest == nil && expected == nil:
		return nil
	case manifest == nil:
		return errors.New("checkpoint manifest has no runtime substrate but restore request provided one")
	case expected == nil:
		return errors.New("checkpoint manifest requires runtime substrate but restore request did not provide one")
	}
	if err := validateRuntimeSubstrateTopology(expected); err != nil {
		return err
	}
	if manifest.Digest != strings.TrimSpace(expected.Digest) {
		return fmt.Errorf("checkpoint manifest substrate digest %s does not match restore substrate digest %s", manifest.Digest, expected.Digest)
	}
	if manifest.Format != strings.TrimSpace(expected.Format) {
		return fmt.Errorf("checkpoint manifest substrate format %s does not match restore substrate format %s", manifest.Format, expected.Format)
	}
	if manifest.BuilderABI != strings.TrimSpace(expected.BuilderABI) {
		return fmt.Errorf("checkpoint manifest substrate builder abi %s does not match restore substrate builder abi %s", manifest.BuilderABI, expected.BuilderABI)
	}
	if manifest.LayoutABI != strings.TrimSpace(expected.LayoutABI) {
		return fmt.Errorf("checkpoint manifest substrate layout abi %s does not match restore substrate layout abi %s", manifest.LayoutABI, expected.LayoutABI)
	}
	return nil
}

func allocateGuestCID() uint32 {
	return 2 + nextGuestCID.Add(1)
}

func jailRootPath(cfg Config, id string) string {
	return filepath.Join(cfg.JailerChrootBaseDir, filepath.Base(cfg.FirecrackerPath), id, "root")
}

func withSnapshotRestore(memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		firecracker.WithSnapshot(memoryPath, statePath)(machine)
		machine.Handlers.FcInit = machine.Handlers.FcInit.Remove(firecracker.AddVsocksHandlerName)
	}
}

func withJailedRestoreFiles(rootfsPath string, scratchDiskPath string, substrateDiskPath string, memoryPath string, statePath string) firecracker.Opt {
	return func(machine *firecracker.Machine) {
		machine.Handlers.Validation = machine.Handlers.Validation.Append(firecracker.JailerConfigValidationHandler)
		machine.Handlers.FcInit = machine.Handlers.FcInit.AppendAfter(firecracker.CreateLogFilesHandlerName, firecracker.Handler{
			Name: "fcinit.LinkHelmrRestoreFilesToRootFS",
			Fn: func(ctx context.Context, machine *firecracker.Machine) error {
				root := jailRootPath(Config{
					FirecrackerPath:     machine.Cfg.JailerCfg.ExecFile,
					JailerChrootBaseDir: machine.Cfg.JailerCfg.ChrootBaseDir,
				}, machine.Cfg.JailerCfg.ID)
				if err := linkIntoJail(rootfsPath, root, filepath.Base(rootfsPath)); err != nil {
					return fmt.Errorf("link rootfs into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == rootfsPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(filepath.Base(rootfsPath))
					}
				}
				if err := linkIntoJailForVMM(scratchDiskPath, root, scratchDiskName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link scratch disk into jail: %w", err)
				}
				for i := range machine.Cfg.Drives {
					if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == scratchDiskPath {
						machine.Cfg.Drives[i].PathOnHost = firecracker.String(scratchDiskName)
					}
				}
				if strings.TrimSpace(substrateDiskPath) != "" {
					substrateName := filepath.Base(substrateDiskPath)
					if err := linkIntoJailForVMM(substrateDiskPath, root, substrateName, *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
						return fmt.Errorf("link substrate disk into jail: %w", err)
					}
					for i := range machine.Cfg.Drives {
						if firecracker.StringValue(machine.Cfg.Drives[i].PathOnHost) == substrateDiskPath {
							machine.Cfg.Drives[i].PathOnHost = firecracker.String(substrateName)
						}
					}
				}
				if err := linkIntoJailForVMM(memoryPath, root, filepath.Base(memoryPath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link snapshot memory into jail: %w", err)
				}
				if err := linkIntoJailForVMM(statePath, root, filepath.Base(statePath), *machine.Cfg.JailerCfg.UID, *machine.Cfg.JailerCfg.GID); err != nil {
					return fmt.Errorf("link snapshot state into jail: %w", err)
				}
				machine.Cfg.Snapshot.MemFilePath = path.Join("/", filepath.Base(memoryPath))
				machine.Cfg.Snapshot.SnapshotPath = path.Join("/", filepath.Base(statePath))
				return nil
			},
		})
	}
}

func linkIntoJailForVMM(source string, root string, name string, uid int, gid int) error {
	if err := linkIntoJail(source, root, name); err != nil {
		return err
	}
	return chownJailFile(filepath.Join(root, name), uid, gid)
}

func linkIntoJail(source string, root string, name string) error {
	dest := filepath.Join(root, name)
	if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(source, dest); err == nil {
		return nil
	}
	if err := cloneSparseFile(source, dest); err == nil {
		return nil
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	return errors.Join(copyErr, closeErr)
}

func cloneSparseFile(source string, dest string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	closed := false
	cleanup := true
	defer func() {
		if !closed {
			_ = output.Close()
		}
		if cleanup {
			_ = os.Remove(dest)
		}
	}()
	if err := output.Truncate(info.Size()); err != nil {
		return err
	}
	if err := copySparseFile(input, output, info.Size()); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	cleanup = false
	return nil
}

func copySparseFile(input *os.File, output *os.File, logicalSize int64) error {
	offset := int64(0)
	buffer := make([]byte, 4<<20)
	for offset < logicalSize {
		dataStart, dataEnd, nextOffset, sparse, err := nextDataRange(input, offset, logicalSize)
		if err != nil {
			return err
		}
		if !sparse {
			return copySparseRange(input, output, buffer, offset, logicalSize)
		}
		if dataStart < dataEnd {
			if err := copySparseRange(input, output, buffer, dataStart, dataEnd); err != nil {
				return err
			}
		}
		offset = nextOffset
	}
	return nil
}

func copySparseRange(input *os.File, output *os.File, buffer []byte, start int64, end int64) error {
	for offset := start; offset < end; {
		remaining := end - offset
		n := int64(len(buffer))
		if remaining < n {
			n = remaining
		}
		chunk := buffer[:n]
		if err := readFullAt(input, chunk, offset); err != nil {
			return err
		}
		if !allZero(chunk) {
			if _, err := output.WriteAt(chunk, offset); err != nil {
				return err
			}
		}
		offset += n
	}
	return nil
}

func chownJailFile(path string, uid int, gid int) error {
	if err := os.Chown(path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
