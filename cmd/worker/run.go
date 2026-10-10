package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/artifact"
	"github.com/helmrdotdev/helmr/internal/artifact/verify"
	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/computerhost"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"github.com/helmrdotdev/helmr/internal/reservation"
	"github.com/helmrdotdev/helmr/internal/vm"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if len(cfg.ComputerDevices) == 0 {
		return errors.New("WORKER_COMPUTER_DEVICES requires an operator-owned NBD allowlist")
	}
	physicalMemoryMiB, err := physicalWorkerMemoryMiB()
	if err != nil {
		return fmt.Errorf("inspect worker host memory: %w", err)
	}
	if err := validateWorkerMemoryMiB(cfg.WorkerCapacityMemoryMiB, physicalMemoryMiB); err != nil {
		return err
	}
	checkpointEncryptor, err := computerhost.NewCheckpointEncryptor(cfg.CheckpointKey)
	if err != nil {
		return fmt.Errorf("configure checkpoint encryption: %w", err)
	}
	log.Info("configured checkpoint encryption", "checkpoint_key_id", checkpointEncryptor.KeyID())
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = defaultWorkDir()
	}
	workDir, jailerDir, err := resolveWorkerRecoveryRoots(workDir, cfg.JailerChrootDir)
	if err != nil {
		return err
	}
	cfg.JailerChrootDir = jailerDir
	networkConfig := firecracker.Config{
		JailerUID:               cfg.JailerUID,
		JailerGID:               cfg.JailerGID,
		StateDir:                filepath.Join(workDir, "vms", "guest"),
		TempDir:                 filepath.Join(workDir, "tmp"),
		NetworkLinkPool:         cfg.NetworkLinkPool,
		NetworkTranslationPool:  cfg.NetworkTranslationPool,
		NetworkResolverIPv4:     cfg.NetworkResolverIPv4,
		NetworkBlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs,
		NetworkCapacity:         int(cfg.WorkerExecutionSlots),
		IPPath:                  cfg.IPPath,
		NFTPath:                 cfg.NFTPath,
	}
	networkReclaimer, err := firecracker.NewNetworkReclaimer(networkConfig)
	if err != nil {
		return fmt.Errorf("configure routed network reclaimer: %w", err)
	}
	var platformStore cas.ImmutableStore
	verifierCgroupRoot, err := worker.PrepareVerifierHost()
	if err != nil {
		return fmt.Errorf("prepare verifier host: %w", err)
	}
	serviceID := uuid.NewV7().String()
	process, err := worker.Acquire(workDir, worker.ProcessIdentity{ServiceID: serviceID})
	if err != nil {
		return fmt.Errorf("acquire worker supervisor singleton: %w", err)
	}
	defer process.Close()
	verifierQualificationRoot := filepath.Join(workDir, "tmp")
	if err := os.MkdirAll(verifierQualificationRoot, 0o700); err != nil {
		return fmt.Errorf("create verifier qualification root: %w", err)
	}
	if err := verify.Qualify(ctx, verifierCgroupRoot, verifierQualificationRoot); err != nil {
		if diagnostic, ok := verify.LocalDiagnostic(err); ok {
			log.Error("deployment artifact verifier qualification failed", "diagnostic", diagnostic)
		}
		return fmt.Errorf("qualify deployment artifact verifier: %w", err)
	}
	if err := os.Remove(filepath.Join(workDir, drainCompleteMarkerName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale drain marker: %w", err)
	}
	var controlPlaneClient *workerclient.Client
	workerHostSecret, err := resolveAuthenticatedWorkerHostSecret(ctx, cfg, workDir, func(hostSecret workerHostSecretFile) error {
		candidate, candidateErr := workerclient.New(cfg.ControlPlaneURL,
			workerclient.WithAuth(hostSecret.WorkerHostID, hostSecret.WorkerHostSecret),
			workerclient.WithService(serviceID),
		)
		if candidateErr != nil {
			return candidateErr
		}
		if candidateErr = candidate.AuthenticateWorker(ctx); candidateErr != nil {
			return candidateErr
		}
		controlPlaneClient = candidate
		return nil
	})
	if err != nil {
		return fmt.Errorf("configure authenticated control client: %w", err)
	}
	reclaimCgroup := func(owner vm.Owner) error {
		return firecracker.RemoveStoppedCgroup(networkConfig.StateDir, cfg.CgroupVersion, owner)
	}
	vmResources := resolveVMResources(cfg)
	hostDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB, cfg.WorkerDiskReserveMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	diskCapacity, err := partitionWorkerDiskCapacity(hostDiskMiB, vmResources.DiskMiB)
	if err != nil {
		return fmt.Errorf("partition worker physical disk capacity: %w", err)
	}
	perSlotDisk, err := computerhost.HostDiskPerSlot(vmResources.MemoryMiB, vmResources.DiskMiB, cfg.ComputerStagingMiB*(1<<20), checkpointEncryptor)
	if err != nil {
		return fmt.Errorf("calculate worker lifecycle disk: %w", err)
	}
	if err := validateWorkerDiskFunding(diskCapacity.HostDiskBytes, perSlotDisk, cfg.WorkerExecutionSlots); err != nil {
		return err
	}
	hostReservations, err := reservation.New(reservation.Vector{
		CPUMillis:     cfg.WorkerCapacityVCPUs * 1000,
		MemoryBytes:   cfg.WorkerCapacityMemoryMiB * 1024 * 1024,
		HostDiskBytes: diskCapacity.HostDiskBytes,
		VMSlots:       int64(cfg.WorkerExecutionSlots),
	})
	if err != nil {
		return fmt.Errorf("configure worker capacity: %w", err)
	}
	imagesDir := cfg.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	guestImageDir := filepath.Join(imagesDir, "guest", "out")
	rootfsPath := filepath.Join(guestImageDir, "rootfs.squashfs")
	connectorConfig := networkConfig
	connectorConfig.PrepareSecretTransport = controlPlaneClient.PrepareSecretTransport
	connectorConfig.FirecrackerPath = cfg.FirecrackerPath
	connectorConfig.CPUTemplateHelperPath = cfg.CPUTemplateHelperPath
	connectorConfig.JailerPath = cfg.JailerPath
	connectorConfig.MkfsExt4Path = cfg.MkfsExt4Path
	connectorConfig.Mke2fsConfigPath = cfg.Mke2fsConfigPath
	connectorConfig.JailerNumaNode = cfg.JailerNumaNode
	connectorConfig.JailerChrootBaseDir = cfg.JailerChrootDir
	connectorConfig.CgroupVersion = cfg.CgroupVersion
	connectorConfig.KernelPath = filepath.Join(guestImageDir, "vmlinuz")
	connectorConfig.InitramfsPath = filepath.Join(guestImageDir, "initramfs")
	connectorConfig.RootfsPath = rootfsPath
	connectorConfig.RuntimeArtifactsPath = filepath.Join(guestImageDir, "runtime-artifacts.json")
	connectorConfig.VCPUCount = cfg.VMVCPUCount
	connectorConfig.MemoryMiB = cfg.VMMemoryMiB
	connectorConfig.ScratchDiskMiB = cfg.VMScratchDiskMiB
	connectorConfig.InitTimeout = cfg.VMInitTimeout
	connectorConfig.HealthTimeout = cfg.VMHealthTimeout
	startupEvidence, connector, err := recoverAndQualifyWorkerRuntime(ctx, log,
		func(recoveryCtx context.Context) (worker.RecoveryEvidence, error) {
			return worker.RecoverLocalVMState(recoveryCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim, reclaimCgroup)
		},
		func() error {
			return computerhost.CheckComputerAttachmentsReleased(filepath.Join(workDir, "tmp"), cfg.ComputerDevices)
		},
		func(qualificationCtx context.Context, evidence worker.RecoveryEvidence) (*firecracker.QualifiedRuntime, error) {
			available, err := availableWorkerDiskBytes(workDir, cfg.WorkerDiskReserveMiB*(1<<20))
			if err != nil {
				return nil, fmt.Errorf("inspect recovered worker disk: %w", err)
			}
			if err := validateWorkerDiskFunding(available, perSlotDisk, cfg.WorkerExecutionSlots); err != nil {
				return nil, fmt.Errorf("fund recovered worker disk: %w", err)
			}
			runtimeCandidate, err := firecracker.NewConnector(connectorConfig)
			if err != nil {
				return nil, fmt.Errorf("configure Firecracker connector: %w", err)
			}
			return qualifyRecoveredRuntime(qualificationCtx, evidence, hostReservations, reservation.Vector{
				CPUMillis:     vmResources.MilliCPU,
				MemoryBytes:   vmResources.MemoryMiB * 1024 * 1024,
				HostDiskBytes: perSlotDisk,
				VMSlots:       1,
			}, runtimeCandidate.Qualify)
		},
	)
	if err != nil {
		return err
	}
	hostRuntimeEvidence := connector.HostRuntimeEvidence()
	runtimeCapabilities := connector.RuntimeCapabilities()
	runtimeArchitecture := definition.RuntimeArchitecture(runtimeCapabilities.Arch)
	if err := artifact.ValidateRuntimeArchitecture(runtimeArchitecture); err != nil {
		return fmt.Errorf("validate Firecracker runtime architecture: %w", err)
	}
	runtimeProfile, cpuShapes, cpuEnvironment, err := workerRuntimeProfile(
		string(runtimeArchitecture), runtimeCapabilities, hostRuntimeEvidence,
	)
	if err != nil {
		return fmt.Errorf("construct Worker runtime profile: %w", err)
	}
	runtimeScratch := filepath.Join(workDir, "tmp", "runtime")
	if err := os.MkdirAll(runtimeScratch, 0o700); err != nil {
		return fmt.Errorf("create runtime scratch: %w", err)
	}
	platformStore, err = cass3.NewImmutable(
		ctx,
		cfg.PlatformStoreURI,
		cass3.WithTempDir(runtimeScratch),
	)
	if err != nil {
		return fmt.Errorf("configure platform artifact store: %w", err)
	}
	store, err := cass3.New(ctx, cfg.CASURI, cass3.WithTempDir(filepath.Join(workDir, "tmp", "cas")))
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	runtimeBackend, err := vm.NewStartLimiter(connector, int(cfg.WorkerExecutionSlots))
	if err != nil {
		return fmt.Errorf("configure host runtime start limit: %w", err)
	}
	allocatable := vm.Resources{
		MilliCPU:  cfg.WorkerCapacityVCPUs * 1000,
		MemoryMiB: cfg.WorkerCapacityMemoryMiB,
	}
	allocatable.Slots = cfg.WorkerExecutionSlots
	workerCapabilities := workerapi.Capabilities{
		Runtime:                   runtimeProfile,
		CPUShapes:                 cpuShapes,
		CPUEnvironment:            cpuEnvironment,
		MaxVCPUs:                  allocatable.MilliCPU / 1000,
		MaxMemoryMiB:              allocatable.MemoryMiB,
		VMMilliCPU:                vmResources.MilliCPU,
		VMMemoryMiB:               vmResources.MemoryMiB,
		GuestEphemeralDiskBytes:   int64(cfg.WorkerExecutionSlots) * diskCapacity.VMGuestEphemeralDiskBytes,
		VMGuestEphemeralDiskBytes: diskCapacity.VMGuestEphemeralDiskBytes,
		ExecutionSlotsAvailable:   int32(allocatable.Slots),
	}
	preparedMachines := computerhost.NewPreparedMachines(runtimeBackend, store, log)
	preparedMachines.CommandLogLimits = &computerv0.CommandLogLimits{ChunkBytes: int32(cfg.LogChunkBytes), BufferBytes: cfg.LogBufferBytes, BufferRecords: int32(cfg.LogBufferRecords)}
	preparedMachines.SessionLogLimits = &agentv1.SessionLogLimits{ChunkBytes: int32(cfg.LogChunkBytes), BufferBytes: cfg.LogBufferBytes, BufferRecords: int32(cfg.LogBufferRecords)}
	preparedMachines.PreparationLogLimits = &computerv0.PreparationLogLimits{ChunkBytes: int32(cfg.LogChunkBytes), BufferBytes: cfg.LogBufferBytes, BufferRecords: int32(cfg.LogBufferRecords)}
	preparedMachines.TempDir = filepath.Join(workDir, "tmp")
	preparedMachines.ComputerObjects = store
	preparedMachines.CheckpointCipher = checkpointEncryptor
	preparedMachines.ComputerRanges = store
	preparedMachines.ComputerDevices = cfg.ComputerDevices
	preparedMachines.ComputerStagingBytes = cfg.ComputerStagingMiB * (1 << 20)
	preparedMachines.ComputerSaveEvery = cfg.ComputerSaveEvery
	preparedMachines.ComputerHelper, err = os.Executable()
	if err != nil {
		return err
	}
	preparedMachines.Reservations = hostReservations
	preparedMachines.PlatformStore = platformStore
	preparedMachines.RuntimeArchitecture = runtimeArchitecture
	preparedMachines.VerifierCgroupRoot = verifierCgroupRoot
	hardAdmission, err := worker.NewHardAdmission(worker.HardAdmissionConfig{
		Probe: worker.SystemHostHealthProbe{
			WorkDir: workDir, CgroupVersion: cfg.CgroupVersion, FirecrackerPath: cfg.FirecrackerPath,
		},
		DiskFloorBytes: admissionDiskFloorMiB(cfg.VMScratchDiskMiB, cfg.WorkerDiskReserveMiB) * 1024 * 1024,
		FDHeadroom:     256,
		DatapathHealth: connector.DatapathHealth,
	})
	if err != nil {
		return fmt.Errorf("configure worker hard admission: %w", err)
	}
	allocations, err := newAllocationConsumer(preparedMachines, controlPlaneClient, int(cfg.WorkerExecutionSlots), cfg.PollEvery, hardAdmission)
	if err != nil {
		return fmt.Errorf("configure allocation consumer: %w", err)
	}
	consumerSpecs := []worker.ConsumerSpec{{
		Name: "allocation", Concurrency: int(cfg.WorkerExecutionSlots),
		ContinueDuringDrain: true, Consumer: allocations,
	}}
	supervisor, err := worker.New(worker.Config{
		ControlPlane: controlPlaneClient, Capabilities: workerCapabilities, Consumers: consumerSpecs,
		Background:         []worker.BackgroundSpec{{Name: "allocation discovery", DrainEligible: true, Run: allocations.RunDiscovery}},
		PollEvery:          cfg.PollEvery,
		AdmissionEvaluator: hardAdmission, Log: log,
		Recover: func(recoveryCtx context.Context) (worker.RecoveryEvidence, error) {
			// Preserve the first inventory, including reclaimed and quarantined
			// owners. Only final drain performs a fresh destructive recovery.
			evidence := startupEvidence
			available, err := availableWorkerDiskBytes(workDir, cfg.WorkerDiskReserveMiB*(1<<20))
			if err != nil {
				return evidence, fmt.Errorf("inspect recovered worker disk at %s: %w", workDir, err)
			}
			if err := validateWorkerDiskFunding(available, perSlotDisk, cfg.WorkerExecutionSlots); err != nil {
				return evidence, fmt.Errorf("recovered worker disk at %s (temporary files at %s): %w", workDir, filepath.Join(workDir, "tmp"), err)
			}
			return evidence, nil
		},
		FinalizeDrain: func(finalizeCtx context.Context) (worker.RecoveryEvidence, error) {
			first, err := worker.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim, reclaimCgroup)
			if err != nil {
				return worker.RecoveryEvidence{}, err
			}
			if len(first.Quarantined) != 0 || len(first.QuarantineErrors) != 0 {
				return first, nil
			}
			// The first pass reclaims any residue. A second complete inventory is
			// the proof submitted to control and therefore must be empty.
			final, err := worker.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim, reclaimCgroup)
			if err != nil {
				return final, err
			}
			return final, computerhost.CheckComputerAttachmentsReleased(filepath.Join(workDir, "tmp"), cfg.ComputerDevices)
		},
		DrainCompleted: func(status workerapi.StatusResponse) error {
			return writeDrainCompleteMarker(workDir, status.WorkerHostID)
		},
	})
	if err != nil {
		return fmt.Errorf("configure worker supervisor: %w", err)
	}
	log.Info("Helmr worker listening", "controlplane_url", cfg.ControlPlaneURL, "worker_host_id", workerHostSecret.WorkerHostID)
	if err := supervisor.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
}

func resolveVMResources(cfg config.Worker) vm.Resources {
	return vm.Resources{
		MilliCPU:  cfg.VMVCPUCount * 1000,
		MemoryMiB: cfg.VMMemoryMiB,
		DiskMiB:   cfg.VMScratchDiskMiB,
		Slots:     1,
	}
}
