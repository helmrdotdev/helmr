package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/builder"
	"github.com/helmrdotdev/helmr/internal/buildkit"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/substrate"
	"github.com/helmrdotdev/helmr/internal/task"
	"github.com/helmrdotdev/helmr/internal/version"
	"github.com/helmrdotdev/helmr/internal/vm"
	workerdaemon "github.com/helmrdotdev/helmr/internal/worker"
)

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	checkpointKey, err := checkpoint.KeyFromBase64(cfg.CheckpointKey)
	if err != nil {
		return fmt.Errorf("load checkpoint encryption key: %w", err)
	}
	checkpointEncryptor, err := checkpoint.New(checkpointKey)
	if err != nil {
		return fmt.Errorf("configure checkpoint encryption: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	supportsRun := slices.Contains(cfg.WorkerRoles, "run")
	supportsBuild := slices.Contains(cfg.WorkerRoles, "build")
	serviceID := uuid.NewString()
	process, err := workerdaemon.Acquire(workDir, workerdaemon.ProcessIdentity{ServiceID: serviceID, Roles: cfg.WorkerRoles})
	if err != nil {
		return fmt.Errorf("acquire worker supervisor singleton: %w", err)
	}
	defer process.Close()
	if err := os.Remove(filepath.Join(workDir, drainCompleteMarkerName)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear stale drain marker: %w", err)
	}
	store, err := cas.NewS3(ctx, cfg.CASURI, cas.WithS3TempDir(filepath.Join(workDir, "tmp", "cas")))
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	var controlClient *client.Client
	workerCredential, err := resolveAuthenticatedWorkerCredential(ctx, cfg, workDir, func(credential workerCredentialFile) error {
		candidate, candidateErr := client.New(cfg.ControlURL,
			client.WithWorkerAuth(credential.WorkerInstanceID, credential.WorkerInstanceSecret),
			client.WithWorkerService(serviceID, api.CurrentWorkerProtocolVersion, supportsRun, supportsBuild),
			client.WithClientIdentity("worker", version.Version),
		)
		if candidateErr != nil {
			return candidateErr
		}
		if candidateErr = candidate.AuthenticateWorker(ctx); candidateErr != nil {
			return candidateErr
		}
		controlClient = candidate
		return nil
	})
	if err != nil {
		return fmt.Errorf("configure authenticated control client: %w", err)
	}
	var imageBuilder builder.Engine
	closeBuilder := func() error { return nil }
	if supportsBuild {
		imageBuilder, closeBuilder, err = buildkit.Open(ctx, buildkit.Config{
			Addr:           cfg.BuildKitAddr,
			OutputRoot:     filepath.Join(workDir, "builds"),
			CacheNamespace: cfg.BuildKitCacheNS,
		})
		if err != nil {
			return fmt.Errorf("configure buildkit: %w", err)
		}
	}
	defer func() {
		if err := closeBuilder(); err != nil {
			log.Warn("close buildkit", "error", err)
		}
	}()
	imagesDir := cfg.ImagesDir
	if imagesDir == "" {
		imagesDir = filepath.Join(workDir, "images")
	}
	guestImageDir := filepath.Join(imagesDir, "guest", "out")
	rootfsPath := filepath.Join(guestImageDir, "rootfs.ext4")
	connector, err := firecracker.NewConnector(firecracker.Config{
		FirecrackerPath:         cfg.FirecrackerPath,
		JailerPath:              cfg.JailerPath,
		JailerUID:               cfg.JailerUID,
		JailerGID:               cfg.JailerGID,
		JailerNumaNode:          cfg.JailerNumaNode,
		JailerChrootBaseDir:     cfg.JailerChrootDir,
		CgroupVersion:           cfg.CgroupVersion,
		KernelPath:              filepath.Join(guestImageDir, "vmlinuz"),
		InitramfsPath:           filepath.Join(guestImageDir, "initramfs"),
		RootfsPath:              rootfsPath,
		RuntimeArtifactsPath:    filepath.Join(guestImageDir, "runtime-artifacts.json"),
		StateDir:                filepath.Join(workDir, "vms", "guest"),
		CNINetworkName:          cfg.CNINetworkName,
		CNIProfile:              cfg.CNIProfile,
		CNIConfDir:              cfg.CNIConfDir,
		CNIBinDir:               cfg.CNIBinDir,
		CNICacheDir:             cfg.CNICacheDir,
		IPPath:                  cfg.IPPath,
		NFTPath:                 cfg.NFTPath,
		NetworkBlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs,
		NetworkBlockedIPv6CIDRs: cfg.NetworkBlockedIPv6CIDRs,
		VCPUCount:               cfg.VMVCPUCount,
		MemoryMiB:               cfg.VMMemoryMiB,
		ScratchDiskMiB:          cfg.VMScratchDiskMiB,
		HealthTimeout:           cfg.VMHealthTimeout,
		HealthAttemptTimeout:    cfg.VMHealthAttemptTimeout,
	})
	if err != nil {
		return fmt.Errorf("configure firecracker connector: %w", err)
	}
	if err := connector.Preflight(ctx); err != nil {
		return fmt.Errorf("firecracker worker preflight: %w", err)
	}
	runtimeCapabilities, err := connector.RuntimeCapabilities()
	if err != nil {
		return fmt.Errorf("inspect firecracker runtime: %w", err)
	}
	runtimeStartLimit := max(int(cfg.WorkerRuntimeStarts), int(cfg.WorkerBuildExecutors))
	runtimeConnector, err := vm.NewStartLimiter(connector, runtimeStartLimit)
	if err != nil {
		return fmt.Errorf("configure host runtime start limit: %w", err)
	}
	hostDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB, cfg.WorkerDiskReserveMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	substrateCacheMaxBytes, artifactCacheMaxBytes := workerCacheBudgetsBytes(cfg.SubstrateCacheMaxMiB, cfg.ArtifactCacheMaxMiB, hostDiskMiB)
	diskCapacity, err := compute.PartitionWorkerDiskCapacity(hostDiskMiB, cfg.VMScratchDiskMiB, substrateCacheMaxBytes+artifactCacheMaxBytes)
	if err != nil {
		return fmt.Errorf("partition worker physical disk capacity: %w", err)
	}
	workerCapabilities := api.WorkerCapabilities{
		ProtocolVersion:         api.CurrentWorkerProtocolVersion,
		WorkerVersion:           version.Version,
		RuntimeID:               runtimeCapabilities.ID,
		RuntimeArch:             runtimeCapabilities.Arch,
		RuntimeABI:              runtimeCapabilities.ABI,
		KernelDigest:            runtimeCapabilities.KernelDigest,
		InitramfsDigest:         runtimeCapabilities.InitramfsDigest,
		RootfsDigest:            runtimeCapabilities.RootfsDigest,
		CNIProfile:              runtimeCapabilities.CNIProfile,
		Region:                  cfg.WorkerProviderRegion,
		Labels:                  cfg.WorkerLabels,
		MaxVCPUs:                cfg.WorkerCapacityVCPUs,
		MaxMemoryMiB:            cfg.WorkerCapacityMemoryMiB,
		VMMilliCPU:              cfg.VMVCPUCount * 1000,
		VMMemoryMiB:             cfg.VMMemoryMiB,
		MaxDiskMiB:              diskCapacity.HostWorkloadMiB,
		VMMaxDiskMiB:            diskCapacity.VMWorkloadDiskMiB,
		ExecutionSlotsAvailable: cfg.WorkerExecutionSlots,
		SupportsRun:             supportsRun,
		SupportsBuild:           supportsBuild,
		MaxBuildExecutors:       cfg.WorkerBuildExecutors,
		MaxRuntimeStarts:        int32(runtimeStartLimit),
		ScratchBytes:            diskCapacity.HostScratchBytes,
		VMMaxScratchBytes:       diskCapacity.VMScratchBytes,
		BuildCacheBytes:         substrateCacheMaxBytes,
		ArtifactCacheBytes:      artifactCacheMaxBytes,
		Network: api.WorkerNetworkCapabilities{
			Internet:      true,
			BlockInternet: true,
			DenyCIDRs:     true,
		},
	}
	compiler := task.GuestCompiler{
		Connector: runtimeConnector,
		TempDir:   filepath.Join(workDir, "tmp"),
	}
	substrateResolver := &substrate.Resolver{
		CacheDir:      filepath.Join(workDir, "substrate-cache"),
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
	if cfg.PreparedRuntimePoolSize > 0 {
		preparedRuntimePool = executor.NewPreparedRuntimePool(workspaceMountConnector, store, cfg.PreparedRuntimePoolSize, log)
		preparedRuntimePool.TempDir = filepath.Join(workDir, "tmp")
		preparedRuntimePool.ArtifactCacheDir = filepath.Join(workDir, "artifact-cache")
		preparedRuntimePool.ArtifactCacheMaxBytes = artifactCacheMaxBytes
		preparedRuntimePool.Substrates = substrateResolver
		preparedRuntimePool.RuntimeSubstrates = controlClient
		preparedRuntimePool.CheckpointEncryptor = checkpointEncryptor
		preparedRuntimePool.Network = compute.DefaultNetworkPolicy()
		preparedRuntimePool.RuntimeInstances = controlClient
		preparedRuntimePool.BackgroundGate = backgroundGate
		log.Info("prepared runtime pool enabled", "pool_size", cfg.PreparedRuntimePoolSize)
	}
	runner, err := workerdaemon.NewRunner(
		controlClient,
		executor.Executor{
			WorkDir: workDir,
			GitPath: cfg.GitPath,
			CAS:     store,
			Builder: imageBuilder,
			RunWaits: executor.ControlRunWaits{
				Client: controlClient,
			},
			Runner: executor.GuestRunner{
				Connector:             runtimeConnector,
				CAS:                   store,
				CheckpointEncryptor:   checkpointEncryptor,
				WorkspaceMounts:       workspaceMountSessions,
				Events:                controlClient,
				TempDir:               filepath.Join(workDir, "tmp"),
				ArtifactCacheDir:      filepath.Join(workDir, "artifact-cache"),
				ArtifactCacheMaxBytes: artifactCacheMaxBytes,
				Substrates:            substrateResolver,
				RuntimeSubstrates:     controlClient,
				Log:                   log,
				Stdout:                os.Stdout,
				Stderr:                os.Stderr,
			},
		},
		workerCapabilities,
		workerdaemon.WithPollEvery(cfg.PollEvery),
		workerdaemon.WithLogger(log),
		workerdaemon.WithDeploymentBuilder(deployment.Builder{
			WorkDir:      workDir,
			CAS:          store,
			Indexer:      deployment.GuestIndexer{Connector: runtimeConnector, TempDir: filepath.Join(workDir, "tmp")},
			Compiler:     compiler,
			ImageBuilder: imageBuilder,
		}),
		workerdaemon.WithMaterializer(executor.WorkspaceMaterializer{
			Connector:             workspaceMountConnector,
			CAS:                   store,
			Sessions:              workspaceMountSessions,
			TempDir:               filepath.Join(workDir, "tmp"),
			ArtifactCacheDir:      filepath.Join(workDir, "artifact-cache"),
			ArtifactCacheMaxBytes: artifactCacheMaxBytes,
			Substrates:            substrateResolver,
			StartupTimeout:        cfg.WorkspaceMountStartupTimeout,
			Log:                   log,
			RuntimePool:           preparedRuntimePool,
			BackgroundGate:        backgroundGate,
		}),
	)
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	consumerSpecs := make([]workerdaemon.ConsumerSpec, 0, 3)
	admission := map[string]int{}
	if supportsRun {
		admission["run"] = int(cfg.WorkerExecutionSlots)
		admission["workspace"] = int(cfg.WorkerExecutionSlots)
		consumerSpecs = append(consumerSpecs,
			workerdaemon.ConsumerSpec{Name: "run", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "run", Consumer: workerdaemon.NewRunConsumer(runner)},
			workerdaemon.ConsumerSpec{Name: "workspace", Concurrency: int(cfg.WorkerExecutionSlots), Admission: "workspace", DrainEligible: true, Consumer: workerdaemon.NewWorkspaceConsumer(runner)},
		)
	}
	if supportsBuild {
		admission["build"] = int(cfg.WorkerBuildExecutors)
		consumerSpecs = append(consumerSpecs, workerdaemon.ConsumerSpec{Name: "build", Concurrency: int(cfg.WorkerBuildExecutors), Admission: "build", Consumer: workerdaemon.NewBuildConsumer(runner)})
	}
	background := make([]workerdaemon.BackgroundSpec, 0, 1)
	if supportsRun && preparedRuntimePool != nil {
		background = append(background, workerdaemon.BackgroundSpec{Name: "runtime-controller", DrainEligible: true, Run: func(runCtx context.Context) error {
			return preparedRuntimePool.ReconcileDesiredRuntimes(runCtx, controlClient)
		}})
	}
	hardAdmission, err := workerdaemon.NewHardAdmission(workerdaemon.HardAdmissionConfig{
		Probe: workerdaemon.SystemHostHealthProbe{
			WorkDir: workDir, CgroupVersion: cfg.CgroupVersion, FirecrackerPath: cfg.FirecrackerPath,
		},
		DiskFloorBytes:   (cfg.WorkerDiskReserveMiB + cfg.VMScratchDiskMiB) * 1024 * 1024,
		FDHeadroom:       256,
		RuntimeSlotCount: cfg.WorkerExecutionSlots,
	})
	if err != nil {
		return fmt.Errorf("configure worker hard admission: %w", err)
	}
	supervisor, err := workerdaemon.New(workerdaemon.Config{
		Control: controlClient, Capabilities: workerCapabilities, Consumers: consumerSpecs, Admission: admission,
		Background: background, PollEvery: cfg.PollEvery, CertificationTTL: cfg.WorkerCertificationTTL,
		AdmissionEvaluator: hardAdmission, Log: log,
		Recover: func(recoveryCtx context.Context) (workerdaemon.RecoveryEvidence, error) {
			return workerdaemon.RecoverLocalRuntimeState(recoveryCtx, workDir, cfg.JailerChrootDir, cfg.IPPath)
		},
		FinalizeDrain: func(finalizeCtx context.Context) (workerdaemon.RecoveryEvidence, error) {
			if err := closePreparedRuntime.Close(finalizeCtx); err != nil {
				return workerdaemon.RecoveryEvidence{}, fmt.Errorf("close prepared runtime pool: %w", err)
			}
			first, err := workerdaemon.RecoverLocalRuntimeState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath)
			if err != nil {
				return workerdaemon.RecoveryEvidence{}, err
			}
			if len(first.Quarantined) != 0 || len(first.QuarantineErrors) != 0 {
				return first, nil
			}
			// The first pass reclaims any residue. A second complete inventory is
			// the proof submitted to control and therefore must be empty.
			return workerdaemon.RecoverLocalRuntimeState(finalizeCtx, workDir, cfg.JailerChrootDir, cfg.IPPath)
		},
		DrainCompleted: func(status api.WorkerStatusResponse) error {
			return writeDrainCompleteMarker(workDir, status.WorkerInstanceID)
		},
	})
	if err != nil {
		return fmt.Errorf("configure worker supervisor: %w", err)
	}
	if preparedRuntimePool != nil {
		preparedRuntimePool.AdmitRuntimeStart = supervisor.AdmitRuntimeStart
	}
	log.Info("helmr worker listening", "control_url", cfg.ControlURL, "worker_instance_id", workerCredential.WorkerInstanceID)
	if err := supervisor.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
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
