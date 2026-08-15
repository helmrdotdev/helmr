package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/cas"
	cass3 "github.com/helmrdotdev/helmr/internal/cas/s3"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/substrate"
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
	checkpointEncryptor, err := checkpoint.New(cfg.CheckpointKey)
	if err != nil {
		return fmt.Errorf("configure checkpoint encryption: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	networkConfig := firecracker.Config{
		JailerUID:               cfg.JailerUID,
		JailerGID:               cfg.JailerGID,
		StateDir:                filepath.Join(workDir, "vms", "guest"),
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
	runtimeCapacity := deriveWorkerRuntimeCapacity(cfg.WorkerExecutionSlots)
	var platformStore cas.ImmutableStore
	verifierCgroupRoot, err := worker.PrepareVerifierHost()
	if err != nil {
		return fmt.Errorf("prepare verifier host: %w", err)
	}
	serviceID := uuid.Must(uuid.NewV7()).String()
	process, err := worker.Acquire(workDir, worker.ProcessIdentity{ServiceID: serviceID})
	if err != nil {
		return fmt.Errorf("acquire worker supervisor singleton: %w", err)
	}
	defer process.Close()
	verifierQualificationRoot := filepath.Join(workDir, "tmp")
	if err := os.MkdirAll(verifierQualificationRoot, 0o700); err != nil {
		return fmt.Errorf("create verifier qualification root: %w", err)
	}
	if err := deployment.QualifyArtifactVerifier(ctx, verifierCgroupRoot, verifierQualificationRoot); err != nil {
		if diagnostic, ok := deployment.VerifierLocalDiagnostic(err); ok {
			log.Error("deployment artifact verifier qualification failed", "diagnostic", diagnostic)
		}
		return fmt.Errorf("qualify deployment artifact verifier: %w", err)
	}
	if err := os.Remove(filepath.Join(workDir, drainCompleteMarkerName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale drain marker: %w", err)
	}
	substrateCacheDir := filepath.Join(workDir, "substrate-cache")
	artifactCacheDir := filepath.Join(workDir, "artifact-cache")
	var controlPlaneClient *workerclient.Client
	workerCredential, err := resolveAuthenticatedWorkerCredential(ctx, cfg, workDir, func(credential workerCredentialFile) error {
		candidate, candidateErr := workerclient.New(cfg.ControlPlaneURL,
			workerclient.WithAuth(credential.WorkerInstanceID, credential.WorkerInstanceSecret),
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
	imagesDir := cfg.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	guestImageDir := filepath.Join(imagesDir, "guest", "out")
	rootfsPath := filepath.Join(guestImageDir, "rootfs.squashfs")
	connectorConfig := networkConfig
	connectorConfig.FirecrackerPath = cfg.FirecrackerPath
	connectorConfig.CPUTemplateHelperPath = cfg.CPUTemplateHelperPath
	connectorConfig.JailerPath = cfg.JailerPath
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
	runtimeCandidate, err := firecracker.NewConnector(connectorConfig)
	if err != nil {
		return fmt.Errorf("configure Firecracker connector: %w", err)
	}
	connector, err := runtimeCandidate.Qualify(ctx)
	if err != nil {
		return fmt.Errorf("qualify Firecracker worker runtime: %w", err)
	}
	hostRuntimeEvidence := connector.HostRuntimeEvidence()
	runtimeCapabilities := connector.RuntimeCapabilities()
	runtimeArchitecture := deployment.RuntimeArchitecture(runtimeCapabilities.Arch)
	if err := deployment.ValidateRuntimeArchitecture(runtimeArchitecture); err != nil {
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
	vmResources := resolveVMResources(cfg)
	runtimeConnector, err := vm.NewStartLimiter(connector, runtimeCapacity.hostStartLimit)
	if err != nil {
		return fmt.Errorf("configure host runtime start limit: %w", err)
	}
	hostDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB, cfg.WorkerDiskReserveMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	substrateCacheMaxBytes, artifactCacheMaxBytes := workerCacheBudgetsBytes(cfg.SubstrateCacheMaxMiB, cfg.ArtifactCacheMaxMiB, hostDiskMiB)
	cacheBytesOnWorkerDisk := substrateCacheMaxBytes + artifactCacheMaxBytes
	diskCapacity, err := compute.PartitionWorkerDiskCapacity(hostDiskMiB, vmResources.DiskMiB, cacheBytesOnWorkerDisk)
	if err != nil {
		return fmt.Errorf("partition worker physical disk capacity: %w", err)
	}
	allocatable := compute.ResourceVector{
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
		GuestEphemeralDiskBytes:   diskCapacity.HostGuestEphemeralDiskBytes,
		VMGuestEphemeralDiskBytes: diskCapacity.VMGuestEphemeralDiskBytes,
		ExecutionSlotsAvailable:   int32(allocatable.Slots),
	}
	workerCapabilities.SubstrateFormat = substrate.Format
	workerCapabilities.SubstrateContract = substrate.Contract
	hostCapacity, err := capacity.New(capacity.Vector{
		CPUMillis:               workerCapabilities.MaxVCPUs * 1000,
		MemoryBytes:             workerCapabilities.MaxMemoryMiB * 1024 * 1024,
		GuestEphemeralDiskBytes: workerCapabilities.GuestEphemeralDiskBytes,
		VMSlots:                 int64(workerCapabilities.ExecutionSlotsAvailable),
	})
	if err != nil {
		return fmt.Errorf("configure worker capacity: %w", err)
	}
	substrateResolver := &substrate.Resolver{
		CacheDir:      substrateCacheDir,
		MkfsExt4Path:  "mkfs.ext4",
		MaxCacheBytes: substrateCacheMaxBytes,
	}
	workspaceMountSessions := executor.NewWorkspaceMountSessions()
	backgroundGate := executor.NewBackgroundWorkGate()
	workspaceMountSessions.BackgroundGate = backgroundGate
	var workspaceMountConnector vm.MaterializingConnector = runtimeConnector
	var preparedRuntimePool *executor.PreparedRuntimePool
	closePreparedRuntime := retryableWorkerCloser{close: func(closeCtx context.Context) error {
		if preparedRuntimePool != nil {
			return preparedRuntimePool.Close(closeCtx)
		}
		return nil
	}}
	defer func() {
		if err := closePreparedRuntime.Close(context.Background()); err != nil {
			log.Warn("prepared runtime pool close failed", "error", err)
		}
	}()
	if runtimeCapacity.preparedPoolSize > 0 {
		preparedRuntimePool = executor.NewPreparedRuntimePool(workspaceMountConnector, store, runtimeCapacity.preparedPoolSize, log)
		preparedRuntimePool.TempDir = filepath.Join(workDir, "tmp")
		preparedRuntimePool.ArtifactCacheDir = artifactCacheDir
		preparedRuntimePool.ArtifactCacheMaxBytes = artifactCacheMaxBytes
		preparedRuntimePool.Substrates = substrateResolver
		preparedRuntimePool.RuntimeSubstrates = controlPlaneClient
		preparedRuntimePool.CheckpointEncryptor = checkpointEncryptor
		preparedRuntimePool.RuntimeInstances = controlPlaneClient
		preparedRuntimePool.BackgroundGate = backgroundGate
		preparedRuntimePool.Capacity = hostCapacity
		preparedRuntimePool.PlatformStore = platformStore
		preparedRuntimePool.RuntimeArchitecture = runtimeArchitecture
		preparedRuntimePool.VerifierCgroupRoot = verifierCgroupRoot
		log.Info("prepared runtime pool enabled", "pool_size", runtimeCapacity.preparedPoolSize)
	}
	runLeaseTasks := executor.ProgramRunner{
		CAS:                 store,
		CheckpointEncryptor: checkpointEncryptor,
		WorkspaceMounts:     workspaceMountSessions,
		TempDir:             filepath.Join(workDir, "tmp"),
	}
	runner, err := worker.NewRunner(
		controlPlaneClient,
		executor.Executor{
			RunLeases:     controlPlaneClient,
			RunLeaseTasks: runLeaseTasks,
		},
		workerCapabilities,
		worker.WithCapacity(hostCapacity),
		worker.WithPollEvery(cfg.PollEvery),
		worker.WithLogger(log),
		worker.WithMaterializer(executor.WorkspaceMaterializer{
			CAS:                   store,
			Sessions:              workspaceMountSessions,
			TempDir:               filepath.Join(workDir, "tmp"),
			ArtifactCacheDir:      artifactCacheDir,
			ArtifactCacheMaxBytes: artifactCacheMaxBytes,
			Log:                   log,
			RuntimePool:           preparedRuntimePool,
			BackgroundGate:        backgroundGate,
		}),
	)
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	consumerSpecs := make([]worker.ConsumerSpec, 0, 4)
	admission := map[string]int{}
	admission["run"] = int(cfg.WorkerExecutionSlots)
	admission["workspace"] = int(cfg.WorkerExecutionSlots)
	consumerSpecs = append(consumerSpecs,
		worker.ConsumerSpec{Name: "run", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "run", ContinueDuringDrain: true, Consumer: worker.NewRunConsumer(runner)},
		worker.ConsumerSpec{Name: "workspace", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "workspace", ContinueDuringDrain: true, BypassAdmissionDuringDrain: true, Consumer: worker.NewWorkspaceConsumer(runner)},
	)
	background := make([]worker.BackgroundSpec, 0, 1)
	if preparedRuntimePool != nil {
		background = append(background, worker.BackgroundSpec{Name: "runtime-controller", DrainEligible: true, Run: func(runCtx context.Context) error {
			return preparedRuntimePool.ReconcileDesiredRuntimes(runCtx, controlPlaneClient)
		}})
	}
	hardAdmission, err := worker.NewHardAdmission(worker.HardAdmissionConfig{
		Probe: worker.SystemHostHealthProbe{
			WorkDir: workDir, CgroupVersion: cfg.CgroupVersion, FirecrackerPath: cfg.FirecrackerPath,
		},
		DiskFloorBytes:   admissionDiskFloorMiB(cfg.VMScratchDiskMiB, cfg.WorkerDiskReserveMiB) * 1024 * 1024,
		FDHeadroom:       256,
		RuntimeSlotCount: cfg.WorkerExecutionSlots,
		DatapathHealth:   connector.DatapathHealth,
	})
	if err != nil {
		return fmt.Errorf("configure worker hard admission: %w", err)
	}
	supervisor, err := worker.New(worker.Config{
		ControlPlane: controlPlaneClient, Capabilities: workerCapabilities, Consumers: consumerSpecs, Admission: admission,
		Background: background, PollEvery: cfg.PollEvery,
		AdmissionEvaluator: hardAdmission, Log: log,
		Recover: func(recoveryCtx context.Context) (worker.RecoveryEvidence, error) {
			var evidence worker.RecoveryEvidence
			var err error
			evidence, err = worker.RecoverLocalVMState(recoveryCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
			if err != nil {
				return evidence, err
			}
			if len(evidence.Quarantined) != len(evidence.QuarantinedOwners) {
				return evidence, errors.New("startup recovery found VM residue without exact ownership")
			}
			for _, owner := range evidence.QuarantinedOwners {
				if owner.Kind != vm.OwnerRuntime {
					continue
				}
				created, err := hostCapacity.Reserve(
					capacity.Key{Kind: "quarantine", Epoch: 1, ID: owner.ID},
					capacity.Vector{
						CPUMillis:               workerCapabilities.VMMilliCPU,
						MemoryBytes:             workerCapabilities.VMMemoryMiB * 1024 * 1024,
						GuestEphemeralDiskBytes: workerCapabilities.VMGuestEphemeralDiskBytes,
						VMSlots:                 1,
					},
				)
				if err != nil {
					return evidence, fmt.Errorf("reserve quarantined runtime capacity: %w", err)
				}
				if !created {
					return evidence, errors.New("quarantined runtime is already reserved")
				}
			}
			return evidence, nil
		},
		FinalizeDrain: func(finalizeCtx context.Context) (worker.RecoveryEvidence, error) {
			if err := closePreparedRuntime.Close(finalizeCtx); err != nil {
				return worker.RecoveryEvidence{}, fmt.Errorf("close prepared runtime pool: %w", err)
			}
			first, err := worker.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
			if err != nil {
				return worker.RecoveryEvidence{}, err
			}
			if len(first.Quarantined) != 0 || len(first.QuarantineErrors) != 0 {
				return first, nil
			}
			// The first pass reclaims any residue. A second complete inventory is
			// the proof submitted to control and therefore must be empty.
			return worker.RecoverLocalVMState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath, networkReclaimer.Reclaim)
		},
		DrainCompleted: func(status workerapi.StatusResponse) error {
			return writeDrainCompleteMarker(workDir, status.WorkerInstanceID)
		},
	})
	if err != nil {
		return fmt.Errorf("configure worker supervisor: %w", err)
	}
	if preparedRuntimePool != nil {
		preparedRuntimePool.AdmitRuntimeStart = supervisor.AdmitRuntimeStart
	}
	log.Info("Helmr worker listening", "controlplane_url", cfg.ControlPlaneURL, "worker_instance_id", workerCredential.WorkerInstanceID)
	if err := supervisor.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
}

type workerRuntimeCapacity struct {
	preparedPoolSize int
	hostStartLimit   int
}

func deriveWorkerRuntimeCapacity(executionSlots int32) workerRuntimeCapacity {
	return workerRuntimeCapacity{
		preparedPoolSize: int(executionSlots),
		hostStartLimit:   int(executionSlots),
	}
}

func resolveVMResources(cfg config.Worker) compute.ResourceVector {
	return compute.ResourceVector{
		MilliCPU:  cfg.VMVCPUCount * 1000,
		MemoryMiB: cfg.VMMemoryMiB,
		DiskMiB:   cfg.VMScratchDiskMiB,
		Slots:     1,
	}
}

type retryableWorkerCloser struct {
	mu     sync.Mutex
	close  func(context.Context) error
	closed bool
}

func (c *retryableWorkerCloser) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	if c.close == nil {
		c.closed = true
		return nil
	}
	if err := c.close(ctx); err != nil {
		return err
	}
	c.closed = true
	return nil
}
