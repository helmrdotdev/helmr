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
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/version"
	workerdaemon "github.com/helmrdotdev/helmr/internal/worker"
)

const defaultDrainTimeout = 30 * time.Minute

const terminationDrainFailedReason = "termination_drain_failed"
const drainCompleteMarkerName = "drain-complete"

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
	identity, err := workerdaemon.ReadProcessIdentity(workDir)
	if err != nil {
		return err
	}
	supportsRun, supportsBuild := identityRoles(identity.Roles)
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), client.WithWorkerService(identity.ServiceID, api.CurrentWorkerProtocolVersion, supportsRun, supportsBuild), client.WithClientIdentity("worker", version.Version))
	if err != nil {
		return fmt.Errorf("configure control client: %w", err)
	}
	status, err := controlClient.DrainWorker(ctx)
	if err != nil {
		return fmt.Errorf("mark worker draining: %w", err)
	}
	log.Info("worker draining", "worker_instance_id", status.WorkerInstanceID, "active_executions", status.ActiveExecutions)
	if !*wait {
		return nil
	}
	if status.Status == api.WorkerStatusDisabled {
		return writeDrainCompleteMarker(workDir, status.WorkerInstanceID)
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
			if status.Status == api.WorkerStatusDisabled {
				log.Info("worker drain completed", "worker_instance_id", status.WorkerInstanceID)
				return writeDrainCompleteMarker(workDir, status.WorkerInstanceID)
			}
		}
	}
}

func writeDrainCompleteMarker(workDir, workerInstanceID string) error {
	path := filepath.Join(workDir, drainCompleteMarkerName)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create drain marker directory: %w", err)
	}
	tmp, err := os.CreateTemp(workDir, ".drain-complete.*")
	if err != nil {
		return fmt.Errorf("create drain marker: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure drain marker: %w", err)
	}
	if _, err := fmt.Fprintln(tmp, workerInstanceID); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write drain marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync drain marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close drain marker: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("install drain marker: %w", err)
	}
	return syncDirectory(workDir)
}

func runFence() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	controlClient, err := workerControlClient()
	if err != nil {
		return err
	}
	if err := controlClient.FenceWorker(ctx, terminationDrainFailedReason); err != nil {
		return fmt.Errorf("persist worker fence: %w", err)
	}
	return nil
}

func workerControlClient() (*client.Client, error) {
	cfg, err := config.LoadWorkerControl()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	workerCredential, err := resolveWorkerControlCredential(cfg, workDir)
	if err != nil {
		return nil, err
	}
	identity, err := workerdaemon.ReadProcessIdentity(workDir)
	if err != nil {
		return nil, err
	}
	supportsRun, supportsBuild := identityRoles(identity.Roles)
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), client.WithWorkerService(identity.ServiceID, api.CurrentWorkerProtocolVersion, supportsRun, supportsBuild), client.WithClientIdentity("worker", version.Version))
	if err != nil {
		return nil, fmt.Errorf("configure control client: %w", err)
	}
	return controlClient, nil
}
