package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/version"
)

const defaultDrainTimeout = 30 * time.Minute

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
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), client.WithClientIdentity("worker", version.Version))
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
