package workergroup

import (
	"context"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Placement shares Group and Pool locks and locks the selected Host.
// Continuation preserves already admitted work through paused/draining supply.
type DispatchSupply struct {
	GroupID         pgtype.UUID
	RegionID        string
	HostID          pgtype.UUID
	Epoch           int64
	RunArchitecture string
	RequirePrimary  bool
	// Continuation continues already admitted work: it accepts a paused or
	// draining Group, a draining Pool or Host and Run pauses. Admission
	// requires active supply without a Run pause.
	Continuation bool
}

type LockedHost struct {
	Group db.WorkerGroup
	Host  db.WorkerHost
	epoch int64
}

func LockDispatchSupply(ctx context.Context, tx pgx.Tx, supply DispatchSupply) (bool, error) {
	var groupActive bool
	err := tx.QueryRow(ctx, `
SELECT status = 'active'
  FROM worker_groups
 WHERE id = $1 AND region_id = $2 AND (status = 'active' OR ($3::boolean AND status IN ('paused', 'draining')))
 FOR SHARE`, supply.GroupID, supply.RegionID, supply.Continuation).Scan(&groupActive)
	if err != nil {
		return false, fmt.Errorf("lock eligible worker group: %w", err)
	}
	var poolActive bool
	err = tx.QueryRow(ctx, `
SELECT worker_pools.status = 'active'
  FROM worker_pools
  JOIN worker_hosts
    ON worker_hosts.worker_pool_id = worker_pools.id
   AND worker_hosts.worker_group_id = worker_pools.worker_group_id
	JOIN worker_groups
	  ON worker_groups.id = worker_pools.worker_group_id
 WHERE worker_hosts.id = $1
   AND worker_hosts.worker_group_id = $2
   AND (worker_pools.status = 'active' OR ($4::boolean AND worker_pools.status = 'draining'))
	AND (NOT $3::boolean OR worker_groups.primary_pool_id = worker_pools.id)
	FOR SHARE OF worker_pools`, supply.HostID, supply.GroupID, supply.RequirePrimary, supply.Continuation).Scan(&poolActive)
	if err != nil {
		return false, fmt.Errorf("lock eligible worker pool: %w", err)
	}

	hostAdmitting, err := db.New(tx).LockRunEligibleWorkerHost(ctx, db.LockRunEligibleWorkerHostParams{
		ID: supply.HostID, WorkerGroupID: supply.GroupID, WorkerEpoch: supply.Epoch,
		Continuation: supply.Continuation, WorkerFreshnessSeconds: ObservationFreshnessSeconds,
		RunArchitecture: supply.RunArchitecture, Contract: vmplatform.Contract,
	})
	if err != nil {
		return false, fmt.Errorf("lock eligible worker epoch: %w", err)
	}
	return groupActive && poolActive && hostAdmitting, nil
}

func CheckClaims(ctx context.Context, q db.DBTX, principal HostPrincipal) error {
	var host, group int64
	if err := q.QueryRow(ctx, `SELECT w.claim_version,g.claim_version FROM worker_hosts w JOIN worker_groups g ON g.id=w.worker_group_id WHERE w.id=$1 AND g.id=$2`, pgvalue.UUID(principal.HostID), pgvalue.UUID(principal.GroupID)).Scan(&host, &group); err != nil {
		return err
	}
	if host != principal.HostClaimVersion || group != principal.GroupClaimVersion {
		return ErrStaleClaims
	}
	return nil
}

func LockHostWithPool(ctx context.Context, q db.Querier, groupID, poolID, hostID uuid.UUID, epoch int64) (LockedHost, error) {
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(groupID))
	if err != nil {
		return LockedHost{}, err
	}
	if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused && group.Status != db.WorkerGroupStatusDraining {
		return LockedHost{}, pgx.ErrNoRows
	}
	pool, err := q.LockWorkerPool(ctx, db.LockWorkerPoolParams{WorkerGroupID: group.ID, WorkerPoolID: pgvalue.UUID(poolID)})
	if err != nil {
		return LockedHost{}, err
	}
	if pool.Status != "active" && pool.Status != "draining" {
		return LockedHost{}, pgx.ErrNoRows
	}
	host, err := q.LockWorkerHostForActivation(ctx, db.LockWorkerHostForActivationParams{WorkerHostID: pgvalue.UUID(hostID), WorkerGroupID: group.ID, WorkerPoolID: pool.ID, WorkerEpoch: pgtype.Int8{Int64: epoch, Valid: true}})
	if err != nil {
		return LockedHost{}, err
	}
	if host.Status != db.WorkerHostStatusActive && host.Status != db.WorkerHostStatusDraining {
		return LockedHost{}, pgx.ErrNoRows
	}
	return LockedHost{Group: group, Host: host, epoch: epoch}, nil
}

func (l LockedHost) Continues() bool {
	return l.Host.CurrentEpoch.Valid && l.Host.CurrentEpoch.Int64 == l.epoch &&
		(l.Host.Status == db.WorkerHostStatusActive || l.Host.Status == db.WorkerHostStatusDraining) &&
		(l.Group.Status == db.WorkerGroupStatusActive || l.Group.Status == db.WorkerGroupStatusPaused || l.Group.Status == db.WorkerGroupStatusDraining)
}
