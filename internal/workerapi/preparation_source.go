package workerapi

import (
	"github.com/helmrdotdev/helmr/internal/bootseed"
	"time"
)

type PreparationSourceRequest struct {
	Network       *NetworkCapabilities `json:"network,omitempty"`
	EnvironmentID string               `json:"environment_id"`
	BuildID       string               `json:"build_id"`
}

type PreparationSource struct {
	WorkerHostID   string                   `json:"worker_host_id"`
	WorkerEpoch    int64                    `json:"worker_epoch"`
	EnvironmentID  string                   `json:"environment_id"`
	BuildID        string                   `json:"build_id"`
	PolicyRevision int64                    `json:"policy_revision"`
	ExpiresAt      time.Time                `json:"expires_at"`
	Manifest       bootseed.Manifest        `json:"manifest"`
	Shape          AllocationShape          `json:"shape"`
	Source         ComputerAllocationSource `json:"source"`
}
