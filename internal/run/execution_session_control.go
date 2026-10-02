package run

import (
	"context"
	"errors"
	"slices"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// LockLiveExecutionForSessionInterruption acquires the union of the caller's
// lineage and the target's owned graph before either side is validated. This
// permits reciprocal controls without acquiring a second Computer out of order.
// The caller locks the Secret union first and owns the transaction.
func LockLiveExecutionForSessionInterruption(ctx context.Context, tx pgx.Tx, fence ExecutionFence, targetID pgtype.UUID) (Execution, OwnedFinalization, error) {
	fail := func(err error) (Execution, OwnedFinalization, error) {
		return Execution{}, OwnedFinalization{}, err
	}
	q := db.New(tx)
	loc, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{ID: fence.LeaseID, LeaseSequence: fence.LeaseSequence, WorkerGroupID: fence.WorkerGroupID, WorkerHostID: fence.WorkerHostID, WorkerEpoch: fence.WorkerEpoch})
	if err != nil {
		return fail(err)
	}
	target, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
	if errors.Is(err, pgx.ErrNoRows) {
		return fail(ErrExecutionTargetNotFound)
	}
	if err != nil {
		return fail(err)
	}
	if !target.CurrentRunID.Valid {
		authority, err := LockLiveExecutionForSession(ctx, tx, fence, targetID)
		if err != nil {
			return fail(err)
		}
		current, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
		if err != nil {
			return fail(err)
		}
		if current.ComputerID != target.ComputerID {
			return fail(cancellationAuthority("session control target Computer changed", nil))
		}
		if current.CurrentRunID.Valid || current.RunGeneration != target.RunGeneration {
			return fail(ErrExecutionTargetChanged)
		}
		return authority, OwnedFinalization{}, nil
	}
	scope := CancellationRequest{OrgID: pgvalue.MustUUIDValue(loc.OrgID), ProjectID: pgvalue.MustUUIDValue(loc.ProjectID), EnvironmentID: pgvalue.MustUUIDValue(loc.EnvironmentID)}
	sourceLineage, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(loc.RunID))
	if err != nil {
		return fail(err)
	}
	targetLineage, err := cancellationLineage(ctx, tx, pgvalue.MustUUIDValue(target.CurrentRunID))
	if err != nil {
		return fail(err)
	}
	descendants, err := discoverOwnedCancellationRuns(ctx, tx, scope, pgvalue.MustUUIDValue(target.CurrentRunID))
	if err != nil {
		return fail(err)
	}
	order := append(slices.Clone(sourceLineage), targetLineage...)
	order = append(order, descendants...)
	slices.SortFunc(order, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	order = slices.Compact(order)
	if len(order) > maxCancellationGraphSize {
		return fail(cancellationAuthority("session control graph exceeds the transaction bound", nil))
	}
	if err = workergroup.LockExecutionHost(ctx, q, fence.host(loc.RegionID, false)); err != nil {
		return fail(err)
	}
	if err = lockExecutionComputers(ctx, tx, order, loc.EnvironmentID, executionTarget{session: targetID}); err != nil {
		return fail(err)
	}
	if err = lockExecutionSessions(ctx, tx, scope, order, targetID); err != nil {
		return fail(err)
	}
	current, err := q.GetSession(ctx, db.GetSessionParams{EnvironmentID: loc.EnvironmentID, ID: targetID})
	if err != nil {
		return fail(err)
	}
	if current.ComputerID != target.ComputerID {
		return fail(cancellationAuthority("session control target Computer changed", nil))
	}
	if current.CurrentRunID != target.CurrentRunID || current.RunGeneration != target.RunGeneration {
		return fail(ErrExecutionTargetChanged)
	}
	for _, id := range order {
		if _, err = lockCancellationRun(ctx, tx, scope, id); err != nil {
			return fail(err)
		}
	}
	reloaded, err := discoverOwnedCancellationRuns(ctx, tx, scope, pgvalue.MustUUIDValue(target.CurrentRunID))
	if err != nil {
		return fail(err)
	}
	if !slices.Equal(descendants, reloaded) {
		return fail(pgx.ErrNoRows)
	}
	for _, lineage := range []struct {
		root uuid.UUID
		ids  []uuid.UUID
	}{
		{pgvalue.MustUUIDValue(loc.RunID), sourceLineage},
		{pgvalue.MustUUIDValue(target.CurrentRunID), targetLineage},
	} {
		current, err := cancellationLineage(ctx, tx, lineage.root)
		if err != nil {
			return fail(err)
		}
		if !slices.Equal(current, lineage.ids) {
			return fail(pgx.ErrNoRows)
		}
	}
	// Re-locking below cannot expand the locked Computer set: target admission
	// and every discovered Run are now locked, and the source operation verifies
	// its lineage.
	graph, err := LockOwnedFinalization(ctx, tx, OwnedFinalizationRequest{OrgID: scope.OrgID, ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, RunID: pgvalue.MustUUIDValue(target.CurrentRunID)})
	if err != nil {
		return fail(err)
	}
	authority, err := LockLiveExecutionForSession(ctx, tx, fence, targetID)
	if err != nil {
		return fail(err)
	}
	return authority, graph, nil
}
