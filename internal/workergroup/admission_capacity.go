package workergroup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const capacityObservationLifetime = 30 * time.Second

var ErrCapacityUnavailable = errors.New("execution capacity is unavailable; retry after Worker capacity becomes available")

// SupplyFingerprint describes topology, not free resources. Ordinary allocation
// is read afresh by the planner; registration and lifecycle changes invalidate
// the provider's relative headroom before it can authorize another admission.
func SupplyFingerprint(ctx context.Context, q db.Querier, group uuid.UUID) (string, error) {
	row, err := q.ReadCapacitySupplyIdentity(ctx, pgvalue.UUID(group))
	if err != nil {
		return "", err
	}
	if !row.Complete {
		return "", ErrCapacityUnavailable
	}
	digest := sha256.Sum256([]byte(row.Identity))
	return hex.EncodeToString(digest[:]), nil
}

// LockExecutionSupply takes step 2 before any Environment, Slack or runtime
// Host lock. All admissions and placements use the same region supply boundary.
func LockExecutionSupply(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT g.id FROM worker_groups g JOIN projects p ON p.default_region_id=g.region_id JOIN environments e ON e.project_id=p.id WHERE e.id=$1 ORDER BY g.id FOR UPDATE OF g`, env)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ObserveCapacityPlan publishes only a provider observation fenced by the exact
// Product topology it observed. Receipt time cannot refresh old provider data.
func ObserveCapacityPlan(ctx context.Context, database db.TxBeginner, group uuid.UUID, request PlanRequest, now time.Time) (PlanResponse, error) {
	var result PlanResponse
	err := db.RunTx(ctx, database, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM worker_groups WHERE id=$1 FOR UPDATE`, group).Scan(&id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrGroupNotFound
			}
			return err
		}
		if len(request.Pools) == 0 || len(request.Pools) > maximumPlanningPools {
			return ErrInvalidPlanRequest
		}
		if request.ObservedAt.IsZero() || request.ObservedAt.After(now) || now.Sub(request.ObservedAt) > capacityObservationLifetime {
			return ErrCapacityUnavailable
		}
		q := db.New(tx)
		fingerprint, err := SupplyFingerprint(ctx, q, group)
		if err != nil {
			return err
		}
		if fingerprint != request.ExpectedSupplyFingerprint {
			return ErrCapacityUnavailable
		}
		result, err = Plan(ctx, q, group, request, now)
		if err != nil {
			return err
		}
		if !result.Complete {
			// Advisory scale-out may still help healthy pools; incomplete evidence
			// cannot refresh the envelope used to accept new work.
			return nil
		}
		var activePools int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM worker_pools WHERE worker_group_id=$1 AND status='active'`, group).Scan(&activePools); err != nil {
			return err
		}
		if activePools != len(request.Pools) {
			return nil
		}
		for _, observed := range request.Pools {
			matched := false
			for _, planned := range result.Pools {
				if planned.PoolID == observed.PoolID && planned.ActiveWorkers == observed.ActiveWorkers && planned.RegisteringWorkers == observed.RegisteringWorkers {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}
		raw, err := json.Marshal(request.Pools)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE worker_groups SET capacity_observed_at=$2,capacity_supply_fingerprint=$3,capacity_pools=$4 WHERE id=$1 AND (capacity_observed_at IS NULL OR capacity_observed_at<=$2)`, group, request.ObservedAt, fingerprint, raw)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrCapacityUnavailable
		}
		return nil
	})
	return result, err
}

var ErrCapacityIncompatible = errors.New("no active Worker pool supports this Computer; configure a compatible Worker pool")

var ErrCapacityExhausted = errors.New("Worker execution capacity is exhausted; raise the Worker maximum or release an unused Computer")

// RequireExecutionPlacement runs with region supply and Environment locks held.
// The candidate's pending episode is already in this transaction. The same check
// after allocation protects earlier accepted claims from a fragmenting placement.
func RequireExecutionPlacement(ctx context.Context, tx pgx.Tx, env, candidate uuid.UUID) error {
	if candidate == uuid.Nil() {
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id JOIN projects p ON p.id=e.project_id WHERE c.execution_deadline_at>clock_timestamp() AND p.default_region_id=(SELECT p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1))`, env).Scan(&pending); err != nil {
			return err
		}
		if !pending {
			return nil
		}
	}
	var group uuid.UUID
	err := tx.QueryRow(ctx, `SELECT g.id FROM worker_groups g JOIN projects p ON p.default_region_id=g.region_id JOIN environments e ON e.project_id=p.id WHERE e.id=$1 AND g.status='active'`, env).Scan(&group)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrCapacityUnavailable
	}
	if err != nil {
		return err
	}
	q := db.New(tx)
	fingerprint, err := SupplyFingerprint(ctx, q, group)
	if err != nil {
		return err
	}
	var raw []byte
	var current bool
	err = tx.QueryRow(ctx, `SELECT capacity_pools,COALESCE(capacity_supply_fingerprint=$2 AND capacity_observed_at>=clock_timestamp()-$3*interval '1 second',false) FROM worker_groups WHERE id=$1`, group, fingerprint, int64(capacityObservationLifetime/time.Second)).Scan(&raw, &current)
	if err != nil {
		return err
	}
	var observed []PoolRequest
	if current {
		if err = json.Unmarshal(raw, &observed); err != nil {
			return err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM worker_pools WHERE worker_group_id=$1 AND status='active' ORDER BY id`, group)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return ErrCapacityUnavailable
	}
	request := PlanRequest{Pools: make([]PoolRequest, 0, len(ids))}
	for _, id := range ids {
		pool := PoolRequest{PoolID: id}
		for _, stored := range observed {
			if stored.PoolID == id {
				pool.MaxAdditionalWorkers = stored.MaxAdditionalWorkers
				break
			}
		}
		request.Pools = append(request.Pools, pool)
	}
	claims, err := db.ReadPendingExecutionClaims(ctx, tx, group)
	if err != nil {
		return err
	}
	store := executionPlanStore{Queries: q, tx: tx, claims: claims}
	// A filtered-out candidate (for example an incompatible checkpoint) cannot
	// turn an empty successful plan into acceptance.
	if candidate != uuid.Nil() {
		g, err := q.GetWorkerGroup(ctx, pgvalue.UUID(group))
		if err != nil {
			return err
		}
		demand, err := store.ListAllocationPlanningDemand(ctx, db.ListAllocationPlanningDemandParams{RegionID: g.RegionID, WorkerGroupID: g.ID, RowLimit: maximumPlanningCandidates + 1})
		if err != nil {
			return err
		}
		found := false
		for _, row := range demand {
			if row.EnvironmentID == pgvalue.UUID(env) && row.OwnerID == pgvalue.UUID(candidate) && row.Kind == "computer" {
				found = true
				break
			}
		}
		if !found {
			return ErrCapacityUnavailable
		}
	}
	plan, err := Plan(ctx, store, group, request, time.Now())
	if err != nil {
		return err
	}
	if !plan.Complete {
		return ErrCapacityUnavailable
	}
	if plan.UnplacedItems != 0 {
		for _, unmatched := range plan.UnmatchedDemand {
			if unmatched.Reason == reasonRuntimeCompatibility || unmatched.Reason == reasonPerInstanceResources || unmatched.Reason == reasonInvalidWorkload {
				return ErrCapacityIncompatible
			}
		}

		if current {
			var stale bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts WHERE worker_group_id=$1 AND status='active' AND (observed_at IS NULL OR observed_at<clock_timestamp()-$2*interval '1 second'))`, group, ObservationFreshnessSeconds).Scan(&stale); err != nil {
				return err
			}
			if !stale {
				return ErrCapacityExhausted
			}
		}
		return ErrCapacityUnavailable
	}
	return nil
}
