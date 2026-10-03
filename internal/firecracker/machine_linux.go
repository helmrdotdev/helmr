//go:build linux

package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/filepack"

	"github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"golang.org/x/sync/errgroup"
)

const stopTimeout = 10 * time.Second

type guestMachine struct {
	mu              sync.Mutex
	computerBarrier chan struct{}
	computerCancel  context.CancelFunc
	computerHeld    bool               // protected by computerBarrier
	checkpointHold  *checkpointCapture // protected by computerBarrier
	stream          vm.Stream
	opened          bool
	closed          bool
	machine         *firecracker.Machine
	machineCancel   context.CancelFunc
	machineExit     *machineExit
	computerExport  *computerExportWatch
	cfg             Config
	kernelArgs      string
	vmPlatform      vmplatform.Profile
	cpuConfigDigest string
	vsockHostPath   string
	instanceDir     string
	jailRoot        string
	scratchDisk     string
	diskFiles       map[string]*os.File
	topology        vm.Topology
	readOnlyDrives  []vm.ReadOnlyDrive
	owner           vm.Owner
	cleaner         vm.Cleaner
	networkBinding  *installedNetworkBinding
	once            sync.Once
	machineStopOnce sync.Once
	machineStopErr  error
	networkErr      error
	err             error
}

func (s *guestMachine) Stream() vm.Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

func (s *guestMachine) Open(ctx context.Context) (vm.Machine, error) {
	if ctx == nil {
		return nil, errors.New("prepared machine open context is nil")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("the Firecracker prepared machine is closed")
	}
	if s.opened {
		s.mu.Unlock()
		return nil, errors.New("the Firecracker prepared machine is already opened")
	}
	s.opened = true
	s.mu.Unlock()

	stream, err := (&Connector{cfg: s.cfg}).connectGuestPort(
		ctx,
		s.vsockHostPath,
		s.machineExit,
	)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.Join(
			errors.New("the Firecracker prepared machine closed while opening"),
			stream.Close(),
		)
	}
	s.stream = stream
	s.mu.Unlock()
	return s, nil
}

func (s *guestMachine) OpenStream(ctx context.Context) (vm.Stream, error) {
	return (&Connector{cfg: s.cfg}).connectGuestPort(ctx, s.vsockHostPath, s.machineExit)
}

func (s *guestMachine) Wait(ctx context.Context) error {
	if s.machineExit == nil {
		return errors.New("the Firecracker machine exit watcher is not configured")
	}
	waitErr := s.machineExit.Wait(ctx)
	s.mu.Lock()
	networkErr := s.networkErr
	s.mu.Unlock()
	var exportErr error
	if s.computerExport != nil {
		exportErr = s.computerExport.failure()
	}
	return errors.Join(waitErr, networkErr, exportErr)
}

func (s *guestMachine) watchNetworkFailure() {
	if s.networkBinding == nil || s.machineExit == nil {
		return
	}
	go func() {
		select {
		case failure := <-s.networkBinding.Failure():
			if failure == nil {
				failure = errors.New("network binding failed without a cause")
			}
			s.mu.Lock()
			s.networkErr = fmt.Errorf("the Firecracker datapath binding failed: %w", failure)
			s.mu.Unlock()
			stopCtx, cancel := context.WithTimeout(context.Background(), stopTimeout)
			defer cancel()
			_ = s.stopMachine(stopCtx)
		case <-s.machineExit.done:
		}
	}()
}

func (s *guestMachine) stopMachine(ctx context.Context) error {
	s.machineStopOnce.Do(func() {
		s.machineStopErr = stopGuestMachine(ctx, s.machine, s.machineExit)
	})
	return s.machineStopErr
}

func (s *guestMachine) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.computerCancel != nil {
		s.computerCancel()
	}
	s.mu.Unlock()
	unlock, err := s.lockComputer(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	s.computerHeld = true
	if s.checkpointHold != nil {
		if err := s.checkpointHold.discardUntransferredSnapshot(); err != nil {
			return err
		}
	}
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		stream := s.stream
		s.mu.Unlock()
		var deactivateErr error
		if s.networkBinding != nil {
			deactivateErr = s.networkBinding.Deactivate()
		}
		stopErr := s.stopMachine(ctx)
		diskFilesErr := closeRuntimeDiskFiles(s.diskFiles)
		if s.machineCancel != nil {
			s.machineCancel()
		}
		var exportErr error
		if s.computerExport != nil {
			exportErr = s.computerExport.join()
		}
		var streamErr error
		if stream != nil {
			streamErr = closeGuestStream(ctx, stream)
		}
		if errors.Is(streamErr, net.ErrClosed) || errors.Is(streamErr, os.ErrClosed) {
			streamErr = nil
		}
		var bindingCloseErr error
		if s.networkBinding != nil {
			bindingCloseErr = s.networkBinding.Close()
		}
		var cleanupErr error
		if bindingCloseErr == nil {
			if s.cleaner != nil {
				cleanupErr = s.cleaner.Cleanup(ctx, s.owner)
			} else {
				cleanupErr = cleanupUnproven(s.owner, errors.New("the Firecracker machine cleaner is not configured"))
			}
		}
		s.err = errors.Join(
			streamErr,
			exportErr,
			diskFilesErr,
			deactivateErr,
			stopErr,
			cleanupErr,
			bindingCloseErr,
		)
	})
	return s.err
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

// The checkpoint handle owns computerBarrier throughout serialization.
func (s *guestMachine) createCheckpointSnapshot(ctx context.Context, request vm.SnapshotRequest) (vm.SnapshotArtifact, error) {
	limits, err := s.SnapshotLimits()
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	checkpointID := safeSnapshotID(request.ID)
	memName := checkpointID + snapshotMemorySuffix
	stateName := checkpointID + snapshotStateSuffix
	memPath := filepath.Join(s.jailRoot, memName)
	statePath := filepath.Join(s.jailRoot, stateName)
	cleanupRawSnapshot := true
	defer func() {
		if cleanupRawSnapshot {
			_ = os.Remove(memPath)
			_ = os.Remove(statePath)
		}
	}()
	var phases []vm.Phase
	recordPhase := func(name string, started time.Time) {
		phases = append(phases, vm.Phase{Name: name, DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started))})
	}
	started := time.Now()
	capturedComputer, err := s.capturePausedComputer(ctx)
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	transferredCapture := false
	defer func() {
		if !transferredCapture {
			capturedComputer.Capture.Release()
		}
	}()
	recordPhase("pause_and_sync_disks", started)
	started = time.Now()
	if err := captureSnapshotState(ctx, s.machine.Cfg.SocketPath, s.jailRoot, memName, stateName, s.cfg.JailerUID, s.cfg.JailerGID); err != nil {
		return vm.SnapshotArtifact{}, fmt.Errorf("create Firecracker snapshot: %w", err)
	}
	recordPhase("firecracker_create_snapshot", started)
	vmPlatform := s.vmPlatform
	expectedRuntimeID, err := vmPlatform.ExpectedID()
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	if vmPlatform.ID != expectedRuntimeID {
		return vm.SnapshotArtifact{}, errors.New("bound host runtime identity is not canonical")
	}
	workerArchitecture := vmPlatform.Arch
	runtimeID := vmPlatform.ID
	kernelDigest := vmPlatform.KernelDigest
	initramfsDigest := vmPlatform.InitramfsDigest
	rootfsDigest := vmPlatform.RootfsDigest
	started = time.Now()
	configDigest, manifest, err := snapshotRuntimeConfig(
		s.cfg,
		checkpointID,
		runtimeID,
		s.cpuConfigDigest,
		kernelDigest,
		initramfsDigest,
		rootfsDigest,
		s.kernelArgs,
		s.topology,
		s.readOnlyDrives,
	)
	if err != nil {
		return vm.SnapshotArtifact{}, err
	}
	if int64(len(manifest)) > limits.ConfigBytes {
		return vm.SnapshotArtifact{}, errors.New("snapshot runtime config exceeds staging limit")
	}
	recordPhase("vm_config_digest", started)
	var scratchFile vm.SnapshotFile
	var memoryFile vm.SnapshotFile
	var scratchPhase vm.Phase
	var memoryPhase vm.Phase
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, s.scratchDisk, filepack.ScratchRole, checkpointID+snapshotScratchPackSuffix, cas.CheckpointScratchDiskMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint scratch disk: %w", err)
		}
		scratchFile = file
		scratchPhase = phase
		return nil
	})
	group.Go(func() error {
		file, phase, err := s.packSnapshotRuntimeFile(groupCtx, memPath, filepack.MemoryRole, checkpointID+snapshotMemoryPackSuffix, cas.CheckpointMemoryMediaType)
		if err != nil {
			return fmt.Errorf("pack checkpoint memory: %w", err)
		}
		memoryFile = file
		memoryPhase = phase
		return nil
	})
	if err := group.Wait(); err != nil {
		removeFiles([]string{scratchFile.Path, memoryFile.Path})
		return vm.SnapshotArtifact{}, err
	}
	phases = append(phases, scratchPhase, memoryPhase)
	if err := os.Remove(memPath); err != nil {
		return vm.SnapshotArtifact{}, fmt.Errorf("remove raw checkpoint memory: %w", err)
	}
	cleanupRawSnapshot = false
	transferredCapture = true
	return vm.SnapshotArtifact{
		Computer:          capturedComputer,
		RuntimeBackend:    "firecracker",
		RuntimeArch:       workerArchitecture,
		VMRuntimeContract: vmPlatform.Contract,
		RuntimeID:         runtimeID,
		KernelDigest:      kernelDigest,
		InitramfsDigest:   initramfsDigest,
		RootfsDigest:      rootfsDigest,
		VMConfigDigest:    configDigest,
		VMVCPUCount:       int32(s.cfg.VCPUCount),
		CPUConfigDigest:   s.cpuConfigDigest,
		VMState:           vm.SnapshotFile{Path: statePath, MediaType: cas.CheckpointVMStateMediaType},
		ScratchDisk:       scratchFile,
		Memory:            []vm.SnapshotFile{memoryFile},
		Manifest:          manifest,
		Phases:            phases,
	}, nil
}

func (s *guestMachine) packSnapshotRuntimeFile(ctx context.Context, sourcePath string, role string, name string, mediaType string) (vm.SnapshotFile, vm.Phase, error) {
	targetPath := filepath.Join(filepath.Dir(s.scratchDisk), name)
	started := time.Now()
	stats, err := filepack.Pack(ctx, sourcePath, targetPath, role)
	if err != nil {
		return vm.SnapshotFile{}, vm.Phase{}, err
	}
	phaseName := "pack_" + strings.ReplaceAll(role, "-", "_") + "_filepack"
	if role == filepack.ScratchRole {
		phaseName = "pack_scratch_filepack"
	}
	measured := vm.FilepackStats(stats)
	return vm.SnapshotFile{Path: targetPath, MediaType: mediaType, Filepack: &measured}, vm.Phase{
		Name:       phaseName,
		DurationMs: vm.RuntimeDurationMilliseconds(time.Since(started)),
		Role:       role,
		MediaType:  mediaType,
		Filepack:   &measured,
	}, nil
}

func stopMachine(ctx context.Context, sdkMachine *firecracker.Machine) error {
	pid, pidErr := sdkMachine.PID()
	stopErr := sdkMachine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := sdkMachine.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find Firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill Firecracker process %d: %w", pid, err))
		} else {
			killWaitCtx, killCancel := context.WithTimeout(context.Background(), stopTimeout)
			waitErr = sdkMachine.Wait(killWaitCtx)
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

func watchMachineExit(sdkMachine *firecracker.Machine) *machineExit {
	exit := &machineExit{done: make(chan struct{})}
	go func() {
		exit.err = sdkMachine.Wait(context.Background())
		close(exit.done)
	}()
	return exit
}

func (e *machineExit) Wait(ctx context.Context) error {
	if e == nil {
		return errors.New("the Firecracker machine exit watcher is not configured")
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

func stopGuestMachine(ctx context.Context, sdkMachine *firecracker.Machine, exit *machineExit) error {
	pid, pidErr := sdkMachine.PID()
	stopErr := sdkMachine.StopVMM()
	waitCtx, cancel := closeContext(ctx, stopTimeout)
	defer cancel()
	waitErr := exit.Wait(waitCtx)
	if errors.Is(waitErr, context.DeadlineExceeded) && pidErr == nil {
		if process, err := os.FindProcess(pid); err != nil {
			waitErr = errors.Join(waitErr, fmt.Errorf("find Firecracker process %d: %w", pid, err))
		} else if err := process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			waitErr = errors.Join(waitErr, fmt.Errorf("kill Firecracker process %d: %w", pid, err))
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

func (s *guestMachine) SnapshotLimits() (vm.SnapshotLimits, error) {
	if s.topology.Computer == nil || s.topology.Computer.ComputerID == "" || s.topology.Computer.SizeBytes <= 0 || s.cfg.MemoryMiB <= 0 || s.cfg.ScratchDiskMiB <= 0 {
		return vm.SnapshotLimits{}, errors.New("checkpoint requires a complete Computer runtime shape")
	}
	if s.cfg.MemoryMiB > math.MaxInt64/(1<<20) || s.cfg.ScratchDiskMiB > math.MaxInt64/(1<<20) {
		return vm.SnapshotLimits{}, errors.New("checkpoint runtime size overflow")
	}
	return vm.SnapshotLimits{ComputerBytes: s.topology.Computer.SizeBytes, MemoryBytes: s.cfg.MemoryMiB * (1 << 20), ScratchBytes: s.cfg.ScratchDiskMiB * (1 << 20), StateBytes: snapshotStateLimit, ConfigBytes: vm.SnapshotConfigLimit}, nil
}
