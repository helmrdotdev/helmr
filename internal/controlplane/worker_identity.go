package controlplane

import (
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// Claims can change after HTTP authentication while a request waits for authority locks.
var errStaleWorkerClaims = errors.New("worker authentication claims are stale")

type workerActor struct {
	WorkerHostID      uuid.UUID
	WorkerGroupID     uuid.UUID
	WorkerEpoch       int64
	ClaimVersion      int64
	GroupClaimVersion int64
	ResourceID        string
	Status            db.WorkerHostStatus
	EpochStartedAt    time.Time
}
