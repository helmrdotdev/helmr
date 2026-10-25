package workergroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dbnotify"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
)

const maxJSONInteger = int64(1<<53 - 1)

// OptionalResources bounds the combined optional work on each Worker.
// Zero prohibits consumption of that dimension; it never means unlimited.
type OptionalResources struct {
	CPUMillis        int64 `json:"cpu_millis"`
	MemoryBytes      int64 `json:"memory_bytes"`
	DiskBytes        int64 `json:"disk_bytes"`
	VMSlots          int32 `json:"vm_slots"`
	MaxTasks         int32 `json:"max_tasks"`
	IOBytesPerSecond int64 `json:"io_bytes_per_second"`
}

type ResourcePolicy struct {
	Optional           OptionalResources `json:"optional"`
	LocalHeadroomSlots int32             `json:"local_headroom_slots"`
}

type PoolResourcePolicy struct {
	WorkerGroupID string `json:"worker_group_id"`
	WorkerPoolID  string `json:"worker_pool_id"`
	Revision      int64  `json:"revision"`
	ResourcePolicy
}

type ResourcePolicyUpdate struct {
	ExpectedRevision int64 `json:"expected_revision"`
	ResourcePolicy
}

type PolicyRevisionConflict struct{ CurrentRevision int64 }

func (e PolicyRevisionConflict) ErrorCode() string { return "resource_policy_revision_conflict" }
func (e PolicyRevisionConflict) ErrorDetails() map[string]json.RawMessage {
	b, _ := json.Marshal(e.CurrentRevision)
	return map[string]json.RawMessage{"current_revision": b}
}
func (e PolicyRevisionConflict) Error() string { return "resource policy revision changed" }

func (p ResourcePolicy) Validate() error {
	for _, n := range []int64{p.Optional.CPUMillis, p.Optional.MemoryBytes, p.Optional.DiskBytes, int64(p.Optional.VMSlots), int64(p.Optional.MaxTasks), p.Optional.IOBytesPerSecond, int64(p.LocalHeadroomSlots)} {
		if n < 0 || n > maxJSONInteger {
			return invalidInput("resource policy limits must be nonnegative safe integers")
		}
	}
	return nil
}

func poolResourcePolicy(p db.WorkerPool) PoolResourcePolicy {
	return PoolResourcePolicy{WorkerGroupID: pgvalue.UUIDString(p.WorkerGroupID), WorkerPoolID: pgvalue.UUIDString(p.ID), Revision: p.ResourcePolicyRevision,
		ResourcePolicy: ResourcePolicy{Optional: OptionalResources{CPUMillis: p.OptionalCpuMillis, MemoryBytes: p.OptionalMemoryBytes, DiskBytes: p.OptionalDiskBytes, VMSlots: p.OptionalVmSlots, MaxTasks: p.OptionalMaxTasks, IOBytesPerSecond: p.OptionalIoBytesPerSecond}, LocalHeadroomSlots: p.LocalHeadroomSlots}}
}

func GetResourcePolicy(ctx context.Context, q db.Querier, groupID, poolID uuid.UUID) (PoolResourcePolicy, error) {
	p, err := q.GetWorkerPool(ctx, db.GetWorkerPoolParams{WorkerGroupID: pgvalue.UUID(groupID), WorkerPoolID: pgvalue.UUID(poolID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return PoolResourcePolicy{}, ErrPoolNotFound
	}
	if err != nil {
		return PoolResourcePolicy{}, err
	}
	return poolResourcePolicy(p), nil
}

// UpdateResourcePolicy serializes with supply admission without changing lifecycle
// claims. The audit and invalidation commit with the policy itself.
func UpdateResourcePolicy(ctx context.Context, txb db.TxBeginner, policy auditpolicy.Policy, actor audit.Evidence, groupID, poolID uuid.UUID, update ResourcePolicyUpdate) (PoolResourcePolicy, error) {
	if err := update.ResourcePolicy.Validate(); err != nil {
		return PoolResourcePolicy{}, err
	}
	if update.ExpectedRevision <= 0 || update.ExpectedRevision > maxJSONInteger {
		return PoolResourcePolicy{}, invalidInput("expected_revision must be a positive safe integer")
	}
	var result PoolResourcePolicy
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := lockGroupForPoolMutation(ctx, q, groupID); err != nil {
			return err
		}
		pool, err := lockPool(ctx, q, groupID, poolID)
		if err != nil {
			return err
		}
		result = poolResourcePolicy(pool)
		if result.Revision != update.ExpectedRevision {
			return PolicyRevisionConflict{result.Revision}
		}
		if !pool.SealedAt.Valid && update.ResourcePolicy != (ResourcePolicy{}) {
			return invalidInput("unsealed pool requires zero resource policy")
		}
		if pool.SealedAt.Valid && (update.Optional.CPUMillis > pool.CapacityCPUMillis.Int64 || update.Optional.MemoryBytes > pool.CapacityMemoryBytes.Int64 || update.Optional.VMSlots > pool.MaxVMSlots.Int32 || update.LocalHeadroomSlots > pool.MaxVMSlots.Int32) {
			return invalidInput("resource policy exceeds pool capacity")
		}
		if result.ResourcePolicy == update.ResourcePolicy {
			return nil
		}
		if result.Revision == maxJSONInteger {
			return conflict("resource policy revision exhausted")
		}
		_, err = tx.Exec(ctx, `UPDATE worker_pools SET resource_policy_revision=resource_policy_revision+1,
 optional_cpu_millis=$3,optional_memory_bytes=$4,optional_disk_bytes=$5,
 optional_vm_slots=$6,optional_max_tasks=$7,optional_io_bytes_per_second=$8,
 local_headroom_slots=$9,updated_at=clock_timestamp() WHERE worker_group_id=$1 AND id=$2`, groupID, poolID, update.Optional.CPUMillis, update.Optional.MemoryBytes, update.Optional.DiskBytes, update.Optional.VMSlots, update.Optional.MaxTasks, update.Optional.IOBytesPerSecond, update.LocalHeadroomSlots)
		if err != nil {
			return fmt.Errorf("update resource policy: %w", err)
		}
		if err := audit.Append(ctx, tx, policy, audit.Event{Evidence: actor, Action: "worker_pool.resource_policy_updated", TargetKind: "worker_pool", TargetID: poolID}); err != nil {
			return err
		}
		result.Revision++
		result.ResourcePolicy = update.ResourcePolicy
		return dbnotify.Notify(ctx, tx, "dispatch", "capacity")
	})
	return result, err
}
