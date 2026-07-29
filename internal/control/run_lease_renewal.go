package control

import (
	"context"
	"errors"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) renewRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	fence api.WorkerRunLeaseFence,
	expectedExpiresAt time.Time,
) (api.WorkerRunLeaseRenewResponse, error) {
	var renewed api.WorkerRunLeaseRenewResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID:                    leaseID,
			LeaseSequence:         fence.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		authority, err := lockRenewableRunLeaseAuthority(
			ctx, work.q, worker, leaseID, fence.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		if (authority.runLease.State != db.RunLeaseStateRunning &&
			authority.runLease.State != db.RunLeaseStateCheckpointing) ||
			!authority.run.ActiveStartedAt.Valid {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseRenewal(fence, authority)
		if err != nil {
			return err
		}
		isCurrent := expectedExpiresAt.Equal(current.ExpiresAt)
		isPrevious := authority.runLease.PreviousExpiresAt.Valid &&
			expectedExpiresAt.Equal(authority.runLease.PreviousExpiresAt.Time)
		if !isCurrent && !isPrevious {
			return errStaleRunLeaseClaim
		}
		now, err := work.q.GetRunLeaseRenewalTime(ctx)
		if err != nil || !now.Valid {
			if err == nil {
				err = errors.New("database transaction time is unavailable")
			}
			return err
		}
		if !now.Time.Before(authority.runLease.ExpiresAt.Time) ||
			!now.Time.Before(authority.workspaceLease.ExpiresAt.Time) {
			return errStaleRunLeaseClaim
		}
		if !authority.run.ActiveStartedAt.Valid ||
			authority.run.MaxActiveDurationMs <= 0 ||
			authority.run.ActiveElapsedMs < 0 ||
			authority.run.ActiveElapsedMs >= authority.run.MaxActiveDurationMs {
			return errStaleRunLeaseClaim
		}
		remaining := authority.run.MaxActiveDurationMs - authority.run.ActiveElapsedMs
		hardDeadline := authority.run.ActiveStartedAt.Time.Add(time.Duration(remaining) * time.Millisecond)
		if !now.Time.Before(hardDeadline) {
			return errStaleRunLeaseClaim
		}
		if isPrevious {
			renewed = current
			return nil
		}
		candidate := now.Time.Add(s.runLeaseTTL)
		if candidate.After(hardDeadline) {
			candidate = hardDeadline
		}
		if !candidate.After(authority.runLease.ExpiresAt.Time) {
			renewed = current
			return nil
		}

		previousExpiry := authority.runLease.ExpiresAt
		authority.runLease, err = work.q.RenewRunLeaseExpiry(ctx, db.RenewRunLeaseExpiryParams{
			RenewedAt:         now,
			ExpiresAt:         pgvalue.Timestamptz(candidate),
			ID:                authority.runLease.ID,
			RunID:             authority.run.ID,
			WorkspaceID:       authority.workspace.ID,
			AttemptNumber:     authority.attempt.Number,
			LeaseSequence:     authority.runLease.LeaseSequence,
			PreviousExpiresAt: previousExpiry,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		authority.workspaceLease, err = work.q.RenewRunWorkspaceLeaseExpiry(ctx, db.RenewRunWorkspaceLeaseExpiryParams{
			RenewedAt:              now,
			ExpiresAt:              pgvalue.Timestamptz(candidate),
			ID:                     authority.workspaceLease.ID,
			WorkspaceID:            authority.workspace.ID,
			RuntimeInstanceID:      authority.runtime.ID,
			WorkspaceMountID:       authority.workspaceMount.ID,
			OwnerRunLeaseID:        authority.runLease.ID,
			OwnershipGeneration:    authority.workspaceLease.OwnershipGeneration,
			WriterGeneration:       authority.workspaceLease.WriterGeneration,
			MountFencingGeneration: authority.workspaceLease.MountFencingGeneration,
			PreviousExpiresAt:      previousExpiry,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		renewed, err = projectRunLeaseRenewal(fence, authority)
		return err
	})
	return renewed, err
}

func projectRunLeaseRenewal(
	fence api.WorkerRunLeaseFence,
	authority runLeaseClaimAuthority,
) (api.WorkerRunLeaseRenewResponse, error) {
	baseWorkspaceVersionID, err := requiredClaimUUIDString(
		"base Workspace version ID",
		authority.workspaceLease.BaseVersionID,
	)
	if err != nil {
		return api.WorkerRunLeaseRenewResponse{}, err
	}
	if !authority.runLease.ExpiresAt.Valid ||
		!authority.workspaceLease.ExpiresAt.Valid ||
		!authority.runLease.ExpiresAt.Time.Equal(authority.workspaceLease.ExpiresAt.Time) {
		return api.WorkerRunLeaseRenewResponse{}, errors.New("Run Lease renewal authority is inconsistent")
	}
	return api.WorkerRunLeaseRenewResponse{
		Lease:                  fence,
		ExpiresAt:              authority.runLease.ExpiresAt.Time.UTC(),
		BaseWorkspaceVersionID: baseWorkspaceVersionID,
	}, nil
}

func lockLiveRunLeaseAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetLiveRunLeaseLocatorsRow,
) (runLeaseClaimAuthority, error) {
	return lockRunLeaseAuthorityForStatuses(
		ctx, q, worker, leaseID, leaseSequence, locators, db.RunStatusRunning,
	)
}

func lockRenewableRunLeaseAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetLiveRunLeaseLocatorsRow,
) (runLeaseClaimAuthority, error) {
	return lockRunLeaseAuthorityForStatuses(
		ctx, q, worker, leaseID, leaseSequence, locators,
		db.RunStatusRunning, db.RunStatusWaiting,
	)
}

func lockRunLeaseAuthorityForStatuses(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetLiveRunLeaseLocatorsRow,
	allowedStatuses ...db.RunStatus,
) (runLeaseClaimAuthority, error) {
	var authority runLeaseClaimAuthority
	var err error
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if err := validateLockedRunLeaseRun(authority.run, leaseID, locators, allowedStatuses...); err != nil {
		return authority, err
	}

	if err := lockRunLeaseWorkspace(ctx, q, &authority, locators); err != nil {
		return authority, err
	}
	if err := lockRunLeaseAttempt(ctx, q, &authority, locators); err != nil {
		return authority, err
	}
	if err := lockRunLeasePhysicalAuthority(
		ctx, q, worker, leaseID, leaseSequence, locators, &authority,
	); err != nil {
		return authority, err
	}
	return authority, nil
}

func validateLockedRunLeaseRun(
	run db.Run,
	leaseID pgtype.UUID,
	locators db.GetLiveRunLeaseLocatorsRow,
	allowedStatuses ...db.RunStatus,
) error {
	statusAllowed := false
	for _, allowed := range allowedStatuses {
		statusAllowed = statusAllowed || run.Status == allowed
	}
	if !statusAllowed ||
		run.CurrentAttemptNumber != locators.AttemptNumber ||
		run.CurrentRunLeaseID != leaseID {
		return errStaleRunLeaseClaim
	}
	return nil
}

func lockRunLeaseWorkspace(
	ctx context.Context,
	q db.Querier,
	authority *runLeaseClaimAuthority,
	locators db.GetLiveRunLeaseLocatorsRow,
) error {
	var err error
	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return errStaleRunLeaseClaim
	}
	return nil
}

func lockRunLeaseAttempt(
	ctx context.Context,
	q db.Querier,
	authority *runLeaseClaimAuthority,
	locators db.GetLiveRunLeaseLocatorsRow,
) error {
	var err error
	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid || authority.attempt.EntrypointKind != authority.run.EntrypointKind {
		return errStaleRunLeaseClaim
	}
	return nil
}

func lockRunLeasePhysicalAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetLiveRunLeaseLocatorsRow,
	authority *runLeaseClaimAuthority,
) error {
	var err error
	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: worker.WorkerGroupID, RegionID: locators.RegionID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if (authority.workerGroup.State != db.WorkerGroupStateActive &&
		authority.workerGroup.State != db.WorkerGroupStateDraining) ||
		!authority.workerGroup.AllowsRun ||
		authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return errStaleRunLeaseClaim
	}

	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return err
	}

	authority.networkSlot, err = q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID: locators.NetworkSlotID, WorkerGroupID: worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		Generation: locators.NetworkSlotGeneration, RuntimeInstanceID: locators.RuntimeInstanceID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if authority.networkSlot.State != db.WorkerNetworkSlotStateBound {
		return errStaleRunLeaseClaim
	}

	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}

	authority.runLease, err = q.LockLiveRunLease(ctx, db.LockLiveRunLeaseParams{
		ID: leaseID, RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber, LeaseSequence: leaseSequence,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if !authority.runLease.StartedAt.Valid || !authority.run.StartedAt.Valid {
		return errStaleRunLeaseClaim
	}
	if err := validateClaimPhysicalAuthority(worker, *authority); err != nil {
		return err
	}

	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID, WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		return staleRunLeaseClaim(err)
	}
	if err := validateRunLeaseWorkspaceAuthority(*authority); err != nil {
		return err
	}
	if !authority.workspaceLease.ExpiresAt.Time.Equal(authority.runLease.ExpiresAt.Time) {
		return errStaleRunLeaseClaim
	}
	return nil
}
