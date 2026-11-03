package workerhost

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/config"
	"github.com/helmrdotdev/helmr/internal/worker"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workerclient"
)

// DrainCompleteMarkerName is the receipt the supervisor writes in its work
// directory once the Control Plane confirms the drained host may stop.
const DrainCompleteMarkerName = "drain-complete"

// DefaultWorkDir is the worker's state root when no work directory is
// configured.
func DefaultWorkDir() string {
	return filepath.Join(os.TempDir(), "helmr-worker")
}

// RunningClient addresses the Control Plane as this machine's running worker:
// its enrolled host, its machine key and the supervisor's current service.
func RunningClient(cfg config.WorkerControlPlane) (*workerclient.Client, Identity, error) {
	workDir := workDirOf(cfg)
	identity, err := ReadIdentity(IdentityPath(workDir, cfg.WorkerIdentityPath))
	if err != nil {
		return nil, Identity{}, err
	}
	process, err := worker.ReadProcessIdentity(workDir)
	if err != nil {
		return nil, Identity{}, err
	}
	client, err := workerclient.New(cfg.ControlPlaneURL, workerclient.WithAuth(identity.WorkerHostID, identity.Key()), workerclient.WithService(process.ServiceID))
	if err != nil {
		return nil, Identity{}, fmt.Errorf("configure control client: %w", err)
	}
	return client, identity, nil
}

func workDirOf(cfg config.WorkerControlPlane) string {
	if cfg.WorkDir == "" {
		return DefaultWorkDir()
	}
	return cfg.WorkDir
}

// Drain marks the running worker draining and, when wait is positive, waits
// up to wait for the supervisor's drain receipt.
func Drain(ctx context.Context, cfg config.WorkerControlPlane, wait time.Duration) (workerapi.StatusResponse, error) {
	client, identity, err := RunningClient(cfg)
	if err != nil {
		return workerapi.StatusResponse{}, err
	}
	status, err := client.DrainWorker(ctx)
	if err != nil {
		return workerapi.StatusResponse{}, fmt.Errorf("mark worker draining: %w", err)
	}
	if wait <= 0 {
		return status, nil
	}
	// The supervisor persists this receipt only after the Control Plane
	// confirms termination_ready. That transition revokes machine keys, so
	// polling the authenticated status endpoint cannot reliably observe it.
	return status, WaitForDrainComplete(ctx, workDirOf(cfg), identity.WorkerHostID, wait, cfg.PollEvery)
}

// Fence records that this machine is gone for good, so the Control Plane
// recovers its work now instead of waiting for it to return.
func Fence(ctx context.Context, cfg config.WorkerControlPlane) error {
	client, _, err := RunningClient(cfg)
	if err != nil {
		return err
	}
	if err := client.FenceWorker(ctx, workerapi.FenceReasonProviderTermination); err != nil {
		return fmt.Errorf("persist worker fence: %w", err)
	}
	return nil
}

// Status reads the running worker's status from the Control Plane.
func Status(ctx context.Context, cfg config.WorkerControlPlane) (workerapi.StatusResponse, error) {
	client, _, err := RunningClient(cfg)
	if err != nil {
		return workerapi.StatusResponse{}, err
	}
	status, err := client.GetWorkerStatus(ctx)
	if err != nil {
		return workerapi.StatusResponse{}, fmt.Errorf("get worker status: %w", err)
	}
	return status, nil
}

func WaitForDrainComplete(ctx context.Context, workDir, workerHostID string, timeout, pollEvery time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		payload, err := os.ReadFile(filepath.Join(workDir, DrainCompleteMarkerName))
		if err == nil {
			if strings.TrimSpace(string(payload)) != workerHostID {
				return errors.New("drain completion marker belongs to a different worker")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("read drain completion marker: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("worker drain is still blocked; observation wait expired and the worker continues draining")
		case <-ticker.C:
		}
	}
}

func WriteDrainComplete(workDir, workerHostID string) error {
	path := filepath.Join(workDir, DrainCompleteMarkerName)
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
	if _, err := fmt.Fprintln(tmp, workerHostID); err != nil {
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
