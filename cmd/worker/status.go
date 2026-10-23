package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/worker"
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
		workDir = defaultWorkDir()
	}
	workerHostSecret, err := resolveWorkerControlPlaneHostSecret(cfg, workDir)
	if err != nil {
		return err
	}
	identity, err := worker.ReadProcessIdentity(workDir)
	if err != nil {
		return err
	}
	controlPlaneClient, err := newControlPlaneClient(cfg.ControlPlaneURL, workerclient.WithAuth(workerHostSecret.WorkerHostID, workerHostSecret.WorkerHostSecret), workerclient.WithService(identity.ServiceID))
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
	if !status.Readiness.Ready {
		return fmt.Errorf("worker admission is not ready: %s", workerPauseReason(status.Readiness))
	}
	log.Info("worker ready", "worker_host_id", status.WorkerHostID, "active_instances", status.ActiveInstances)
	return nil
}

func workerPauseReason(readiness workerapi.Readiness) string {
	if readiness.PausedReason == "" {
		return "unavailable"
	}
	return readiness.PausedReason
}
