package controlplane

import (
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
)

// Claims can change after HTTP authentication while a request waits for authority locks.
//
// Computer worker authority is the locked host row (current_epoch equals the
// authenticated epoch, allowed status), the locked group row (allowed status),
// the Computer fences (writer generation, desired version, capture checkpoint and
// checkpoint status) and deadlines checked after the final lock. Claim versions
// only signal credential freshness: an existing comparison that fails returns
// errStaleWorkerClaims so the worker re-authenticates (401) and replays; it is
// never a fence. Lock-free Secret proxy snapshots report the comparison as a
// column of the same snapshot. Paths without a comparison (capture, restore
// acknowledgement, instance observations) intentionally rely on those fences alone.
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

// checkLockedClaims compares the authenticated claim versions with Worker host
// and group rows the caller has locked.
func (w workerActor) checkLockedClaims(host db.WorkerHost, group db.WorkerGroup) error {
	if host.ClaimVersion != w.ClaimVersion || group.ClaimVersion != w.GroupClaimVersion {
		return errStaleWorkerClaims
	}
	return nil
}
