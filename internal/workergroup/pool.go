package workergroup

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerpoolname"
	"github.com/jackc/pgx/v5"
)

// PrimarySelection is the worker group after a primary pool selection and the
// selected pool. Applied is false when the pool already was the primary.
type PrimarySelection struct {
	Group   db.WorkerGroup
	Pool    db.WorkerPool
	Applied bool
}

// ListPools returns a worker group and its pools.
func ListPools(ctx context.Context, q db.Querier, groupID uuid.UUID) (db.WorkerGroup, []db.WorkerPool, error) {
	group, err := GetGroup(ctx, q, groupID)
	if err != nil {
		return db.WorkerGroup{}, nil, err
	}
	pools, err := q.ListWorkerPools(ctx, pgvalue.UUID(groupID))
	if err != nil {
		return db.WorkerGroup{}, nil, fmt.Errorf("list worker pools: %w", err)
	}
	return group, pools, nil
}

// CreatePool adds a pending pool to an active or paused worker group fenced by
// the group's claim version, and returns the group with the new pool.
func CreatePool(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, name string, expectedGroupClaimVersion int64) (db.WorkerGroup, db.WorkerPool, error) {
	if err := workerpoolname.Validate(name); err != nil {
		return db.WorkerGroup{}, db.WorkerPool{}, InputError{message: err.Error()}
	}
	if expectedGroupClaimVersion <= 0 {
		return db.WorkerGroup{}, db.WorkerPool{}, invalidInput("expected_group_claim_version must be positive")
	}
	var group db.WorkerGroup
	var created db.WorkerPool
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		group, err = lockGroupForPoolMutation(ctx, q, groupID)
		if err != nil {
			return err
		}
		if group.ClaimVersion != expectedGroupClaimVersion ||
			(group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused) {
			return conflict("worker group state or claim version changed")
		}
		created, err = q.CreatePendingWorkerPool(ctx, db.CreatePendingWorkerPoolParams{
			WorkerPoolID: pgvalue.UUID(uuid.NewV7()), Name: name,
			WorkerGroupID: pgvalue.UUID(groupID), ExpectedGroupClaimVersion: expectedGroupClaimVersion,
		})
		if db.IsUniqueViolation(err) {
			return conflict("worker pool name is already in use")
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker group state or claim version changed")
		}
		if err != nil {
			return fmt.Errorf("create worker pool: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.WorkerGroup{}, db.WorkerPool{}, err
	}
	return group, created, nil
}

// SelectPrimaryPool makes an active, sealed pool the primary pool that fresh
// work routes to. It is the single authority for that change: administrators
// and provider controllers both call it, so lock order and replay semantics
// cannot drift. Selecting the current primary again is a replay that succeeds
// unless the expected claim version is ahead of the group's. A zero poolID is
// rejected after the group is locked and found selectable.
func SelectPrimaryPool(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, poolID uuid.UUID, expectedGroupClaimVersion int64) (PrimarySelection, error) {
	if expectedGroupClaimVersion <= 0 {
		return PrimarySelection{}, invalidInput("expected_group_claim_version must be positive")
	}
	var selection PrimarySelection
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		group, err := lockGroupForPoolMutation(ctx, q, groupID)
		if err != nil {
			return err
		}
		if group.Status != db.WorkerGroupStatusActive && group.Status != db.WorkerGroupStatusPaused {
			return conflict("worker group is not active for primary selection")
		}
		if poolID == uuid.Nil() {
			return invalidInput("primary pool is required")
		}
		pool, err := lockPool(ctx, q, groupID, poolID)
		if err != nil {
			return err
		}
		if pool.Status != "active" || !pool.SealedAt.Valid {
			return conflict("selected worker pool is not active and sealed")
		}
		selection.Pool = pool
		if group.PrimaryPoolID.Valid && group.PrimaryPoolID.Bytes == poolID {
			if expectedGroupClaimVersion > group.ClaimVersion {
				return conflict("expected group claim version is in the future")
			}
			selection.Group = group
			return nil
		}
		if expectedGroupClaimVersion != group.ClaimVersion {
			return conflict("worker group state, claim version, or primary selection changed")
		}
		group, err = q.SetWorkerGroupPrimaryPool(ctx, db.SetWorkerGroupPrimaryPoolParams{
			PoolID: pgvalue.UUID(poolID), WorkerGroupID: pgvalue.UUID(groupID),
			ExpectedGroupClaimVersion: expectedGroupClaimVersion,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker group state, claim version, or primary selection changed")
		}
		if err != nil {
			return fmt.Errorf("set worker group primary pool: %w", err)
		}
		selection.Group = group
		selection.Applied = true
		return nil
	})
	if err != nil {
		return PrimarySelection{}, err
	}
	return selection, nil
}

// DrainPool starts draining an active pool fenced by its claim version, and
// returns the group with the pool.
func DrainPool(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, poolID uuid.UUID, expectedPoolClaimVersion int64) (db.WorkerGroup, db.WorkerPool, error) {
	return transitionPool(ctx, txb, groupID, poolID, expectedPoolClaimVersion, "draining")
}

// DisablePool disables an unreferenced pending or drained pool fenced by its
// claim version, and returns the group with the pool.
func DisablePool(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, poolID uuid.UUID, expectedPoolClaimVersion int64) (db.WorkerGroup, db.WorkerPool, error) {
	return transitionPool(ctx, txb, groupID, poolID, expectedPoolClaimVersion, "disabled")
}

// transitionPool moves a pool to target. Replaying a transition with the claim
// version that fenced it succeeds without applying it again.
func transitionPool(ctx context.Context, txb db.TxBeginner, groupID uuid.UUID, poolID uuid.UUID, expectedPoolClaimVersion int64, target string) (db.WorkerGroup, db.WorkerPool, error) {
	if expectedPoolClaimVersion <= 0 {
		return db.WorkerGroup{}, db.WorkerPool{}, invalidInput("expected_pool_claim_version must be positive")
	}
	var group db.WorkerGroup
	var pool db.WorkerPool
	err := db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		q := db.New(tx)
		var err error
		group, err = lockGroupForPoolMutation(ctx, q, groupID)
		if err != nil {
			return err
		}
		pool, err = lockPool(ctx, q, groupID, poolID)
		if err != nil {
			return err
		}
		if pool.Status == target && pool.ClaimVersion == expectedPoolClaimVersion+1 {
			return nil
		}
		if pool.ClaimVersion != expectedPoolClaimVersion {
			return conflict("worker pool state or claim version changed")
		}
		if target == "draining" && pool.Status != "active" {
			return conflict("only an active worker pool can begin draining")
		}
		if target == "disabled" && pool.Status != "pending" && pool.Status != "draining" {
			return conflict("only an unreferenced pending or drained worker pool can be disabled")
		}
		pool, err = q.TransitionWorkerPoolLifecycle(ctx, db.TransitionWorkerPoolLifecycleParams{
			TargetStatus: target, WorkerPoolID: pgvalue.UUID(poolID), WorkerGroupID: pgvalue.UUID(groupID),
			ExpectedPoolClaimVersion: expectedPoolClaimVersion,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return conflict("worker pool is primary, referenced, or required for retained execution")
		}
		if err != nil {
			return fmt.Errorf("transition worker pool lifecycle: %w", err)
		}
		return nil
	})
	if err != nil {
		return db.WorkerGroup{}, db.WorkerPool{}, err
	}
	return group, pool, nil
}

// lockGroupForPoolMutation locks the worker group row FOR UPDATE ahead of its
// pools.
func lockGroupForPoolMutation(ctx context.Context, q db.Querier, groupID uuid.UUID) (db.WorkerGroup, error) {
	group, err := q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(groupID))
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkerGroup{}, ErrGroupNotFound
	}
	if err != nil {
		return db.WorkerGroup{}, fmt.Errorf("lock worker group for pool mutation: %w", err)
	}
	return group, nil
}

// lockPool locks one pool of a worker group FOR UPDATE.
func lockPool(ctx context.Context, q db.Querier, groupID uuid.UUID, poolID uuid.UUID) (db.WorkerPool, error) {
	pool, err := q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
		WorkerGroupID: pgvalue.UUID(groupID), WorkerPoolID: pgvalue.UUID(poolID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.WorkerPool{}, ErrPoolNotFound
	}
	if err != nil {
		return db.WorkerPool{}, fmt.Errorf("lock worker pool: %w", err)
	}
	return pool, nil
}
