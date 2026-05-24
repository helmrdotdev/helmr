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
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/taskbundle"
	"github.com/helmrdotdev/helmr/internal/worker"
)

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadWorker()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store, err := cas.NewS3(ctx, cfg.CASURI)
	if err != nil {
		return fmt.Errorf("configure CAS: %w", err)
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
	workerCredential, err := resolveWorkerInstanceCredential(ctx, cfg, workDir)
	if err != nil {
		return err
	}
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret))
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
		RootfsPath:              filepath.Join(guestImageDir, "rootfs.ext4"),
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
	workerDiskMiB, err := advertisedWorkerDiskMiB(workDir, cfg.WorkerDiskMiB)
	if err != nil {
		return fmt.Errorf("inspect worker disk capacity: %w", err)
	}
	if workerDiskMiB > cfg.VMScratchDiskMiB {
		workerDiskMiB = cfg.VMScratchDiskMiB
	}
	workerCapabilities := api.WorkerCapabilities{
		RuntimeArch:             runtimeCapabilities.Arch,
		RuntimeABI:              runtimeCapabilities.ABI,
		KernelDigest:            runtimeCapabilities.KernelDigest,
		RootfsDigest:            runtimeCapabilities.RootfsDigest,
		CNIProfile:              runtimeCapabilities.CNIProfile,
		Region:                  cfg.WorkerRegion,
		Labels:                  cfg.WorkerLabels,
		MaxVCPUs:                runtimeCapabilities.VCPUCount,
		MaxMemoryMiB:            runtimeCapabilities.MemoryMiB,
		MaxDiskMiB:              workerDiskMiB,
		ExecutionSlotsAvailable: 1,
	}
	status, err := controlClient.ActivateWorker(ctx, workerCapabilities)
	if err != nil {
		return fmt.Errorf("activate worker: %w", err)
	}
	log.Info("worker activated", "worker_instance_id", status.WorkerInstanceID, "status", status.Status, "active_executions", status.ActiveExecutions)
	compiler := taskbundle.GuestCompiler{
		Connector: connector,
		TempDir:   filepath.Join(workDir, "tmp"),
	}
	runner, err := worker.NewRunner(
		controlClient,
		executor.Executor{
			WorkDir: workDir,
			GitPath: cfg.GitPath,
			CAS:     store,
			Builder: builder,
			Waitpoints: executor.ControlWaitpoints{
				Client: controlClient,
			},
			Runner: executor.GuestRunner{
				Connector:           connector,
				CAS:                 store,
				CheckpointEncryptor: checkpointEncryptor,
				Events:              controlClient,
				TempDir:             filepath.Join(workDir, "tmp"),
				Stdout:              os.Stdout,
				Stderr:              os.Stderr,
			},
		},
		workerCapabilities,
		worker.WithPollEvery(cfg.PollEvery),
		worker.WithLogger(log),
		worker.WithDeploymentBuilder(deployment.Builder{
			WorkDir:  workDir,
			CAS:      store,
			Indexer:  deployment.GuestIndexer{Connector: connector, TempDir: filepath.Join(workDir, "tmp")},
			Compiler: compiler,
		}),
	)
	if err != nil {
		return fmt.Errorf("configure worker: %w", err)
	}
	log.Info("helmr worker listening", "control_url", cfg.ControlURL, "worker_instance_id", workerCredential.WorkerInstanceID)
	if err := runner.Run(ctx); err != nil && err != context.Canceled {
		return err
	}
	return nil
}
