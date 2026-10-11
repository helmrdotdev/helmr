package workergroup

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

// ErrStaleClaims reports that a host's or group's claim version changed after
// HTTP authentication while a request waited for authority locks.
//
// Computer worker authority is the locked host row (current_epoch equals the
// authenticated epoch, allowed status), the locked group row (allowed status),
// the Computer fences (writer generation, desired version, capture checkpoint and
// checkpoint status) and deadlines checked after the final lock. Claim versions
// only signal credential freshness: an existing comparison that fails returns
// ErrStaleClaims so the worker re-authenticates (401) and replays; it is never
// a fence. Lock-free Secret proxy snapshots report the comparison as a column
// of the same snapshot. Paths without a comparison (capture, restore
// acknowledgement, instance observations) intentionally rely on those fences
// alone.
var ErrStaleClaims = errors.New("worker authentication claims are stale")

// HostPrincipal is a worker host authenticated by its epoch credential: the
// host and group it belongs to, the epoch the credential was minted for, and
// the host and group claim versions it carried.
type HostPrincipal struct {
	HostID            uuid.UUID
	GroupID           uuid.UUID
	Epoch             int64
	HostClaimVersion  int64
	GroupClaimVersion int64
	ResourceID        string
	Status            db.WorkerHostStatus
	EpochStartedAt    time.Time
}

// checkLockedClaims compares the authenticated claim versions with the worker
// host and group rows the caller has locked.
func (p HostPrincipal) checkLockedClaims(host db.WorkerHost, group db.WorkerGroup) error {
	if host.ClaimVersion != p.HostClaimVersion || group.ClaimVersion != p.GroupClaimVersion {
		return ErrStaleClaims
	}
	return nil
}

// LockRuntimeHost locks the group then host for read-only runtime admission.
// The caller must retain these shared locks through its operation. A false
// result rejects lifecycle or epoch changes; stale credentials and missing rows
// retain their distinct errors so each caller can apply its denial semantics.
func LockRuntimeHost(ctx context.Context, tx pgx.Tx, host HostPrincipal) (bool, error) {
	var groupClaim, hostClaim int64
	var groupStatus, hostStatus string
	var epoch *int64
	if err := tx.QueryRow(ctx, `SELECT claim_version,status FROM worker_groups WHERE id=$1 FOR SHARE`, host.GroupID).Scan(&groupClaim, &groupStatus); err != nil {
		return false, err
	}
	if err := tx.QueryRow(ctx, `SELECT claim_version,current_epoch,status FROM worker_hosts WHERE id=$1 AND worker_group_id=$2 FOR SHARE`, host.HostID, host.GroupID).Scan(&hostClaim, &epoch, &hostStatus); err != nil {
		return false, err
	}
	if groupClaim != host.GroupClaimVersion || hostClaim != host.HostClaimVersion {
		return false, ErrStaleClaims
	}
	return epoch != nil && *epoch == host.Epoch && (hostStatus == "active" || hostStatus == "draining") && (groupStatus == "active" || groupStatus == "paused" || groupStatus == "draining"), nil
}
