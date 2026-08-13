package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerGroupPrimaryResult struct {
	group   db.WorkerGroup
	pools   map[string]db.WorkerPool
	applied bool
}

type workerGroupPrimarySelectionCommand struct {
	workerGroupID             string
	expectedGroupClaimVersion int64
	desired                   func(db.WorkerGroup) (pgtype.UUID, error)
}

// reconcileWorkerGroupPrimarySelection is the single Product authority for
// changing fresh-work routing. Provider controllers and Admin transports both
// delegate here so lock order and replay semantics cannot drift.
func (s *Server) reconcileWorkerGroupPrimarySelection(
	ctx context.Context,
	command workerGroupPrimarySelectionCommand,
) (workerGroupPrimaryResult, error) {
	result := workerGroupPrimaryResult{pools: make(map[string]db.WorkerPool)}
	err := s.inTx(ctx, func(work *txWork) error {
		group, err := work.q.LockWorkerGroupForPoolMutation(ctx, command.workerGroupID)
		if isNoRows(err) {
			return notFound(errors.New("worker group not found"))
		}
		if err != nil {
			return errors.New("lock worker group for primary selection")
		}
		if group.State != db.WorkerGroupStateActive && group.State != db.WorkerGroupStatePaused {
			return conflict(errors.New("worker group is not active for primary selection"))
		}
		desired, err := command.desired(group)
		if err != nil {
			return err
		}
		if !desired.Valid {
			return badRequest(errors.New("primary pool is required"))
		}
		pool, err := work.q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: command.workerGroupID,
			WorkerPoolID:  desired,
		})
		if isNoRows(err) {
			return notFound(errors.New("worker pool not found"))
		}
		if err != nil {
			return errors.New("lock worker pool for primary selection")
		}
		if pool.State != "active" || !pool.SealedAt.Valid {
			return conflict(errors.New("selected worker pool is not active and sealed"))
		}
		result.pools[pgvalue.UUIDString(pool.ID)] = pool
		if optionalUUIDEqual(group.PrimaryPoolID, desired) {
			if command.expectedGroupClaimVersion > group.ClaimVersion {
				return conflict(errors.New("expected group claim version is in the future"))
			}
			result.group = group
			return nil
		}
		if command.expectedGroupClaimVersion != group.ClaimVersion {
			return conflict(errors.New("worker group state, claim version, or primary selection changed"))
		}
		group, err = work.q.SetWorkerGroupPrimaryPool(ctx, db.SetWorkerGroupPrimaryPoolParams{
			PoolID: desired, WorkerGroupID: command.workerGroupID,
			ExpectedGroupClaimVersion: command.expectedGroupClaimVersion,
		})
		if isNoRows(err) {
			return conflict(errors.New("worker group state, claim version, or primary selection changed"))
		}
		if err != nil {
			return errors.New("set worker group primary pool")
		}
		result.group = group
		result.applied = true
		return nil
	})
	return result, err
}

func optionalUUIDEqual(left, right pgtype.UUID) bool {
	return left.Valid == right.Valid && (!left.Valid || left.Bytes == right.Bytes)
}
