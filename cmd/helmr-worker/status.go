package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/executor"
	workerdaemon "github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

func runStatus(log *slog.Logger) error {
	cfg, err := config.LoadWorkerControlPlane()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	workDir := cfg.WorkDir
	if workDir == "" {
		workDir = executor.DefaultWorkDir()
	}
	workerCredential, err := resolveWorkerControlPlaneCredential(cfg, workDir)
	if err != nil {
		return err
	}
	identity, err := workerdaemon.ReadProcessIdentity(workDir)
	if err != nil {
		return err
	}
	supportsRun, supportsBuild := identityRoles(identity.Roles)
	controlPlaneClient, err := workerclient.New(cfg.ControlPlaneURL, workerclient.WithAuth(workerCredential.WorkerInstanceID, workerCredential.WorkerInstanceSecret), workerclient.WithService(identity.ServiceID, workerapi.CurrentProtocolVersion, supportsRun, supportsBuild))
	if err != nil {
		return fmt.Errorf("configure control client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := controlPlaneClient.GetWorkerStatus(ctx)
	if err != nil {
		return fmt.Errorf("get worker status: %w", err)
	}
	if status.Status != workerapi.StatusActive {
		return fmt.Errorf("worker status is %s", status.Status)
	}
	if supportsRun {
		if status.Readiness.Run == nil || !status.Readiness.Run.Ready {
			return fmt.Errorf("worker run role is not ready: %s", workerPauseReason(status.Readiness.Run))
		}
		if status.Readiness.Runtime == nil || !status.Readiness.Runtime.Ready {
			return fmt.Errorf("worker runtime role is not ready: %s", workerPauseReason(status.Readiness.Runtime))
		}
	}
	if supportsBuild && (status.Readiness.Build == nil || !status.Readiness.Build.Ready) {
		return fmt.Errorf("worker build role is not ready: %s", workerPauseReason(status.Readiness.Build))
	}
	log.Info("worker ready", "worker_instance_id", status.WorkerInstanceID, "active_executions", status.ActiveExecutions)
	return nil
}

func workerPauseReason(readiness *workerapi.RoleReadiness) string {
	if readiness == nil || readiness.PausedReason == "" {
		return "unavailable"
	}
	return readiness.PausedReason
}
