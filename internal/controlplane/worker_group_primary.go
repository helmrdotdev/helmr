package controlplane

import (
	"context"
	"errors"
	"sort"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerGroupPrimarySelection struct {
	runPoolID   pgtype.UUID
	buildPoolID pgtype.UUID
}

type workerGroupPrimaryResult struct {
	group   db.WorkerGroup
	pools   map[string]db.WorkerPool
	applied bool
}

type workerGroupPrimarySelectionCommand struct {
	workerGroupID             string
	expectedGroupClaimVersion int64
	requireCompleteSelection  bool
	desired                   func(db.WorkerGroup) (workerGroupPrimarySelection, error)
}

// reconcileWorkerGroupPrimarySelection is the single Product authority for
// changing fresh-work routing. Provider controllers and Admin transports both
// delegate here so lock order, replay semantics, and role validation cannot
// drift. Provider resource identities never cross this boundary.
func (s *Server) reconcileWorkerGroupPrimarySelection(
	ctx context.Context,
	command workerGroupPrimarySelectionCommand,
) (workerGroupPrimaryResult, error) {
	var result workerGroupPrimaryResult
	result.pools = make(map[string]db.WorkerPool)
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
		if err := validateWorkerGroupPrimaryRoleShape(group, desired, command.requireCompleteSelection); err != nil {
			return err
		}

		poolIDs := distinctPrimaryPoolIDs(group, desired)
		for _, poolID := range poolIDs {
			pool, err := work.q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
				WorkerGroupID: command.workerGroupID,
				WorkerPoolID:  poolID,
			})
			if isNoRows(err) {
				return notFound(errors.New("worker pool not found"))
			}
			if err != nil {
				return errors.New("lock worker pool for primary selection")
			}
			if pool.ID != poolID || pool.WorkerGroupID != command.workerGroupID {
				return conflict(errors.New("worker pool binding changed"))
			}
			result.pools[pgvalue.UUIDString(pool.ID)] = pool
		}
		if err := validateSelectedPrimaryPools(desired, result.pools); err != nil {
			return err
		}

		if primarySelectionMatches(group, desired) {
			if command.expectedGroupClaimVersion > group.ClaimVersion {
				return conflict(errors.New("expected group claim version is in the future"))
			}
			result.group = group
			return nil
		}
		if command.expectedGroupClaimVersion != group.ClaimVersion {
			return conflict(errors.New("worker group state, claim version, or primary selection changed"))
		}
		group, err = work.q.SetWorkerGroupPrimaryPools(ctx, db.SetWorkerGroupPrimaryPoolsParams{
			RunPoolID:                 desired.runPoolID,
			BuildPoolID:               desired.buildPoolID,
			WorkerGroupID:             command.workerGroupID,
			ExpectedGroupClaimVersion: command.expectedGroupClaimVersion,
		})
		if isNoRows(err) {
			return conflict(errors.New("worker group state, claim version, or primary selection changed"))
		}
		if err != nil {
			return errors.New("set worker group primary pools")
		}
		result.group = group
		result.applied = true
		return nil
	})
	return result, err
}

func validateWorkerGroupPrimaryRoleShape(
	group db.WorkerGroup,
	desired workerGroupPrimarySelection,
	requireComplete bool,
) error {
	if desired.runPoolID.Valid && !group.AllowsRun || desired.buildPoolID.Valid && !group.AllowsBuild {
		return badRequest(errors.New("primary selection includes a role the worker group does not allow"))
	}
	if requireComplete && (group.AllowsRun != desired.runPoolID.Valid || group.AllowsBuild != desired.buildPoolID.Valid) {
		return badRequest(errors.New("primary selection must specify exactly one pool for every allowed worker group role"))
	}
	return nil
}

func validateSelectedPrimaryPools(desired workerGroupPrimarySelection, pools map[string]db.WorkerPool) error {
	for _, selection := range []struct {
		id          pgtype.UUID
		requireRole func(db.WorkerPool) bool
	}{
		{id: desired.runPoolID, requireRole: func(pool db.WorkerPool) bool { return pool.AllowsRun }},
		{id: desired.buildPoolID, requireRole: func(pool db.WorkerPool) bool { return pool.AllowsBuild }},
	} {
		if !selection.id.Valid {
			continue
		}
		pool, ok := pools[pgvalue.UUIDString(selection.id)]
		if !ok {
			return notFound(errors.New("selected worker pool was not found"))
		}
		if pool.State != "active" || !pool.SealedAt.Valid {
			return conflict(errors.New("selected worker pool is not active and sealed"))
		}
		if !selection.requireRole(pool) {
			return badRequest(errors.New("selected worker pool does not support its primary role"))
		}
	}
	return nil
}

func distinctPrimaryPoolIDs(group db.WorkerGroup, desired workerGroupPrimarySelection) []pgtype.UUID {
	byID := make(map[string]pgtype.UUID)
	for _, id := range []pgtype.UUID{
		group.PrimaryRunPoolID, group.PrimaryBuildPoolID, desired.runPoolID, desired.buildPoolID,
	} {
		if id.Valid {
			byID[pgvalue.UUIDString(id)] = id
		}
	}
	keys := make([]string, 0, len(byID))
	for key := range byID {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]pgtype.UUID, 0, len(keys))
	for _, key := range keys {
		result = append(result, byID[key])
	}
	return result
}

func primarySelectionMatches(group db.WorkerGroup, desired workerGroupPrimarySelection) bool {
	return optionalUUIDEqual(group.PrimaryRunPoolID, desired.runPoolID) &&
		optionalUUIDEqual(group.PrimaryBuildPoolID, desired.buildPoolID)
}

func optionalUUIDEqual(left, right pgtype.UUID) bool {
	return left.Valid == right.Valid && (!left.Valid || left.Bytes == right.Bytes)
}
