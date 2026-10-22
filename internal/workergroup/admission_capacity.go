package workergroup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pglock"
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
// Host lock. Admissions and placements serialize on the region's advisory gate;
// shared Group locks exclude topology changes without blocking runtime renewal.
func LockExecutionSupply(ctx context.Context, tx pgx.Tx, env uuid.UUID) error {
	var region string
	if err := tx.QueryRow(ctx, `SELECT p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1`, env).Scan(&region); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pglock.Key("helmr:execution-supply:"+region)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM worker_groups WHERE region_id=$1 ORDER BY id FOR SHARE`, region)
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

var ErrCapacityExhausted = errors.New("worker execution capacity is exhausted; raise the Worker maximum or release an unused Computer")

// ExecutionPlacement records the complete placement result before an admission
// or allocation changes resources. Only this transaction may compare it later.
type ExecutionPlacement struct {
	results   map[string]string
	confirmed bool
}

// ReadExecutionPlacement runs under the region supply gate. An already blocked
// claim remains visible, but does not forbid unrelated work that preserves every
// currently feasible claim.
func ReadExecutionPlacement(ctx context.Context, tx pgx.Tx, env uuid.UUID) (ExecutionPlacement, error) {
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM computers c JOIN environments e ON e.id=c.environment_id JOIN projects p ON p.id=e.project_id WHERE c.execution_deadline_at>clock_timestamp() AND p.default_region_id=(SELECT p.default_region_id FROM environments e JOIN projects p ON p.id=e.project_id WHERE e.id=$1))`, env).Scan(&pending); err != nil {
		return ExecutionPlacement{}, err
	}
	if !pending {
		return ExecutionPlacement{}, nil
	}
	var group uuid.UUID
	err := tx.QueryRow(ctx, `SELECT g.id FROM worker_groups g JOIN projects p ON p.default_region_id=g.region_id JOIN environments e ON e.project_id=p.id WHERE e.id=$1 AND g.status='active'`, env).Scan(&group)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExecutionPlacement{}, ErrCapacityUnavailable
	}
	if err != nil {
		return ExecutionPlacement{}, err
	}
	q := db.New(tx)
	fingerprint, err := SupplyFingerprint(ctx, q, group)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	var raw []byte
	var current bool
	err = tx.QueryRow(ctx, `SELECT capacity_pools,COALESCE(capacity_supply_fingerprint=$2 AND capacity_observed_at>=clock_timestamp()-$3*interval '1 second',false) FROM worker_groups WHERE id=$1`, group, fingerprint, int64(capacityObservationLifetime/time.Second)).Scan(&raw, &current)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	var observed []PoolRequest
	if current {
		if err = json.Unmarshal(raw, &observed); err != nil {
			return ExecutionPlacement{}, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM worker_pools WHERE worker_group_id=$1 AND status='active' ORDER BY id`, group)
	if err != nil {
		return ExecutionPlacement{}, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return ExecutionPlacement{}, err
	}
	if len(ids) == 0 {
		return ExecutionPlacement{}, ErrCapacityUnavailable
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
		return ExecutionPlacement{}, err
	}
	store := executionPlanStore{Queries: q, tx: tx, claims: claims}
	plan, err := Plan(ctx, store, group, request, time.Now())
	if err != nil {
		return ExecutionPlacement{}, err
	}
	if !plan.Complete {
		return ExecutionPlacement{}, ErrCapacityUnavailable
	}
	if current {
		var stale bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM worker_hosts WHERE worker_group_id=$1 AND status='active' AND (observed_at IS NULL OR observed_at<clock_timestamp()-$2*interval '1 second'))`, group, ObservationFreshnessSeconds).Scan(&stale); err != nil {
			return ExecutionPlacement{}, err
		}
		current = !stale
	}
	return ExecutionPlacement{results: plan.PlacementResults, confirmed: current}, nil
}

// RequireExecutionPlacement compares the same serialized supply before and after
// mutation. A new candidate must place; already accepted feasible claims may not
// be displaced. A claim fulfilled by physical allocation may leave pending demand.
func RequireExecutionPlacement(ctx context.Context, tx pgx.Tx, env, candidate uuid.UUID, before ExecutionPlacement) error {
	after, err := ReadExecutionPlacement(ctx, tx, env)
	if err != nil {
		return err
	}
	reject := func(reason string) error {
		switch reason {
		case reasonRuntimeCompatibility, reasonPerInstanceResources, reasonInvalidWorkload:
			return ErrCapacityIncompatible
		}
		if after.confirmed {
			return ErrCapacityExhausted
		}
		return ErrCapacityUnavailable
	}
	if candidate != uuid.Nil() {
		key := fmt.Sprintf("computer:%x:%x", pgvalue.UUID(env).Bytes, pgvalue.UUID(candidate).Bytes)
		reason, found := after.results[key]
		if !found {
			return ErrCapacityUnavailable
		}
		if reason != "" {
			return reject(reason)
		}
	}
	for key, prior := range before.results {
		if reason, remains := after.results[key]; prior == "" && remains && reason != "" {
			return reject(reason)
		}
	}
	return nil
}
