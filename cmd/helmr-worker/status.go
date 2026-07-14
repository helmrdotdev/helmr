package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/client"
	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/executor"
	"github.com/helmrdotdev/helmr/internal/version"
	workerdaemon "github.com/helmrdotdev/helmr/internal/worker"
)

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
	identity, err := workerdaemon.ReadProcessIdentity(workDir)
	if err != nil {
		return err
	}
	supportsRun, supportsBuild := identityRoles(identity.Roles)
	controlClient, err := client.New(cfg.ControlURL, client.WithWorkerAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), client.WithWorkerService(identity.ServiceID, api.CurrentWorkerProtocolVersion, supportsRun, supportsBuild), client.WithClientIdentity("worker", version.Version))
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
