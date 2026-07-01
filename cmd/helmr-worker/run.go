package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/helmrdotdev/helmr/internal/api"
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
	"github.com/helmrdotdev/helmr/internal/worker"
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
	store, err := cas.NewS3(ctx, cfg.CASURI, cas.WithS3TempDir(filepath.Join(workDir, "tmp", "cas")))
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
	}
	workerCredential, err := resolveWorkerInstanceCredential(ctx, cfg, workDir)
	if err != nil {
		return err
	}
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), client.WithClientIdentity("worker", version.Version))
	if err != nil {
		return fmt.Errorf("configure control client: %w", err)
	}
	builder, closeBuilder, err := buildkit.Open(ctx, buildkit.Config{
		Addr:           cfg.BuildKitAddr,
		OutputRoot:     filepath.Join(workDir, "builds"),
		CacheNamespace: cfg.BuildKitCacheNS,
	})
	if err != nil {
		return fmt.Errorf("configure buildkit: %w", err)
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
	hostDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	workerDiskMiB := min(hostDiskMiB, cfg.VMScratchDiskMiB)
	substrateCacheMaxBytes, artifactCacheMaxBytes := workerCacheBudgetsBytes(cfg.SubstrateCacheMaxMiB, cfg.ArtifactCacheMaxMiB, hostDiskMiB)
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
		Region:                  cfg.WorkerRegion,
		Labels:                  cfg.WorkerLabels,
		MaxVCPUs:                cfg.WorkerCapacityVCPUs,
		MaxMemoryMiB:            cfg.WorkerCapacityMemoryMiB,
		MaxDiskMiB:              workerDiskMiB,
		ExecutionSlotsAvailable: cfg.WorkerExecutionSlots,
		Network: api.WorkerNetworkCapabilities{
			Internet:      true,
			BlockInternet: true,
			DenyCIDRs:     true,
		},
	}
	status, err := controlClient.ActivateWorker(ctx, workerCapabilities)
	if err != nil {
		return fmt.Errorf("activate worker: %w", err)
	}
	log.Info("worker activated", "worker_instance_id", status.WorkerInstanceID, "status", status.Status, "active_executions", status.ActiveExecutions)
	compiler := task.GuestCompiler{
		Connector: connector,
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
	var workspaceMountConnector vm.MaterializingConnector = connector
	var preparedBaseConnector *firecracker.PreparedBaseConnector
	if cfg.PreparedBasePoolSize > 0 {
		preparedBaseConnector = firecracker.NewPreparedBaseConnector(connector, cfg.PreparedBasePoolSize, log)
		preparedBaseConnector.BackgroundGate = backgroundGate
		preparedBaseConnector.Start(ctx, compute.DefaultNetworkPolicy())
		defer func() {
			if err := preparedBaseConnector.Close(context.Background()); err != nil {
				log.Warn("prepared base connector close failed", "error", err)
			}
		}()
		workspaceMountConnector = preparedBaseConnector
		log.Info("prepared base connector enabled", "pool_size", cfg.PreparedBasePoolSize)
	}
	var preparedRuntimePool *executor.PreparedRuntimePool
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
		defer func() {
			if err := preparedRuntimePool.Close(context.Background()); err != nil {
				log.Warn("prepared runtime pool close failed", "error", err)
			}
		}()
		log.Info("prepared runtime pool enabled", "pool_size", cfg.PreparedRuntimePoolSize)
	}
	workspaceMountSessions.RuntimePool = preparedRuntimePool
	runner, err := worker.NewRunner(
		controlClient,
		executor.Executor{
			WorkDir: workDir,
			GitPath: cfg.GitPath,
			CAS:     store,
			Builder: builder,
			RunWaits: executor.ControlRunWaits{
				Client: controlClient,
			},
			Runner: executor.GuestRunner{
				Connector:             connector,
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
		worker.WithPollEvery(cfg.PollEvery),
		worker.WithLogger(log),
		worker.WithDeploymentBuilder(deployment.Builder{
			WorkDir:      workDir,
			CAS:          store,
			Indexer:      deployment.GuestIndexer{Connector: connector, TempDir: filepath.Join(workDir, "tmp")},
			Compiler:     compiler,
			ImageBuilder: builder,
		}),
		worker.WithMaterializer(executor.WorkspaceMaterializer{
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
	if preparedRuntimePool != nil {
		go func() {
			if err := preparedRuntimePool.FollowWarmCommands(ctx, controlClient, workerCapabilities); err != nil && err != context.Canceled {
				log.Warn("prepared runtime warm command follower stopped", "error", err)
			}
		}()
	}
	log.Info("helmr worker listening", "control_url", cfg.ControlURL, "worker_instance_id", workerCredential.WorkerInstanceID)
	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
}
