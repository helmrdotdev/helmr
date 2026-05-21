package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/buildkit"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/checkpoint"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/firecracker"
	"github.com/helmrdotdev/helmr/internal/worker"
	"golang.org/x/sys/unix"
)

const defaultDrainTimeout = 30 * time.Minute

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "drain":
			if err := runDrain(log, os.Args[2:]); err != nil {
				log.Error("drain worker", "error", err)
				os.Exit(1)
			}
			return
		case "status":
			if err := runStatus(log); err != nil {
				log.Error("get worker status", "error", err)
				os.Exit(1)
			}
			return
		default:
			log.Error("unknown command", "command", os.Args[1])
			os.Exit(1)
		}
	}
	if err := run(log); err != nil {
		log.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

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
			Compiler: executor.GuestCompiler{
				Connector: connector,
				TempDir:   filepath.Join(workDir, "tmp"),
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

func advertisedWorkerDiskMiB(workDir string, configuredMiB int64) (int64, error) {
	if configuredMiB > 0 {
		return configuredMiB, nil
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return 0, err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(workDir, &stat); err != nil {
		return 0, err
	}
	availableMiB := int64((stat.Bavail * uint64(stat.Bsize)) / (1024 * 1024))
	if availableMiB <= 0 {
		return 0, errors.New("worker filesystem has no available disk capacity")
	}
	reserveMiB := availableMiB / 10
	if reserveMiB < 1024 {
		reserveMiB = 1024
	}
	if reserveMiB >= availableMiB {
		reserveMiB = availableMiB / 2
	}
	advertisedMiB := availableMiB - reserveMiB
	if advertisedMiB <= 0 {
		return 0, errors.New("worker filesystem has no advertisable disk capacity")
	}
	return advertisedMiB, nil
}

func runDrain(log *slog.Logger, args []string) error {
	flags := flag.NewFlagSet("drain", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	timeout := flags.Duration("timeout", defaultDrainTimeout, "maximum time to wait for active executions to finish")
	wait := flags.Bool("wait", true, "wait until this worker has no active executions")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.LoadWorkerControl()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	workerCredential, err := resolveWorkerControlCredential(cfg, workDir)
	if err != nil {
		return err
	}
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret))
	if err != nil {
		return fmt.Errorf("configure control client: %w", err)
	}
	status, err := controlClient.DrainWorker(ctx)
	if err != nil {
		return fmt.Errorf("mark worker draining: %w", err)
	}
	log.Info("worker draining", "worker_instance_id", status.WorkerInstanceID, "active_executions", status.ActiveExecutions)
	if !*wait || status.ActiveExecutions == 0 {
		return nil
	}
	deadline := time.NewTimer(*timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(cfg.PollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("worker drain timed out with %d active executions", status.ActiveExecutions)
		case <-ticker.C:
			status, err = controlClient.GetWorkerStatus(ctx)
			if err != nil {
				return fmt.Errorf("get worker drain status: %w", err)
			}
			log.Info("worker drain status", "worker_instance_id", status.WorkerInstanceID, "status", status.Status, "active_executions", status.ActiveExecutions)
			if status.ActiveExecutions == 0 {
				return nil
			}
		}
	}
}

func runStatus(log *slog.Logger) error {
	cfg, err := config.LoadWorkerControl()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	workerCredential, err := resolveWorkerControlCredential(cfg, workDir)
	if err != nil {
		return err
	}
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret))
	if err != nil {
		return fmt.Errorf("configure control client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := controlClient.GetWorkerStatus(ctx)
	if err != nil {
		return fmt.Errorf("get worker status: %w", err)
	}
	if status.Status != api.WorkerStatusActive {
		return fmt.Errorf("worker status is %s", status.Status)
	}
	log.Info("worker active", "worker_instance_id", status.WorkerInstanceID, "active_executions", status.ActiveExecutions)
	return nil
}
