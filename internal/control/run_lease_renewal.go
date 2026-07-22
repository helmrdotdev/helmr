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
	request api.WorkerRunLeaseReceipt,
) (api.WorkerRunLeaseReceipt, error) {
	var renewed api.WorkerRunLeaseReceipt
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID:                    leaseID,
			LeaseSequence:         request.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		authority, err := lockRenewableRunLeaseAuthority(
			ctx, work.q, worker, leaseID, request.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		if (authority.runLease.State != db.RunLeaseStateRunning &&
			authority.runLease.State != db.RunLeaseStateCheckpointing) ||
			!authority.run.ActiveStartedAt.Valid {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run:            authority.run,
			attempt:        authority.attempt,
			runtime:        authority.runtime,
			networkSlot:    authority.networkSlot,
			runLease:       authority.runLease,
			workspace:      authority.workspace,
			workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil {
			return err
		}
		isCurrent := equalRunLeaseReceipt(current, request)
		isPrevious := false
		if authority.runLease.PreviousExpiresAt.Valid {
			previous := current
			previous.ExpiresAt = authority.runLease.PreviousExpiresAt.Time
			isPrevious = equalRunLeaseReceipt(previous, request)
		}
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
		renewed, err = projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run:            authority.run,
			attempt:        authority.attempt,
			runtime:        authority.runtime,
			networkSlot:    authority.networkSlot,
			runLease:       authority.runLease,
			workspace:      authority.workspace,
			workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		return err
	})
	return renewed, err
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
	statusAllowed := false
	for _, allowed := range allowedStatuses {
		statusAllowed = statusAllowed || authority.run.Status == allowed
	}
	if !statusAllowed ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
		return authority, errStaleRunLeaseClaim
	}

	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return authority, errStaleRunLeaseClaim
	}

	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid || authority.attempt.EntrypointKind != authority.run.EntrypointKind {
		return authority, errStaleRunLeaseClaim
	}

	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: worker.WorkerGroupID, RegionID: locators.RegionID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if (authority.workerGroup.State != db.WorkerGroupStateActive &&
		authority.workerGroup.State != db.WorkerGroupStateDraining) ||
		!authority.workerGroup.AllowsRun ||
		authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return authority, errStaleRunLeaseClaim
	}

	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return authority, err
	}

	authority.networkSlot, err = q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID: locators.NetworkSlotID, WorkerGroupID: worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		Generation: locators.NetworkSlotGeneration, RuntimeInstanceID: locators.RuntimeInstanceID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if authority.networkSlot.State != db.WorkerNetworkSlotStateBound {
		return authority, errStaleRunLeaseClaim
	}

	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}

	authority.runLease, err = q.LockLiveRunLease(ctx, db.LockLiveRunLeaseParams{
		ID: leaseID, RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber, LeaseSequence: leaseSequence,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if !authority.runLease.StartedAt.Valid || !authority.run.StartedAt.Valid {
		return authority, errStaleRunLeaseClaim
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return authority, err
	}

	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID, WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		return authority, staleRunLeaseClaim(err)
	}
	if err := validateRunLeaseWorkspaceAuthority(authority); err != nil {
		return authority, err
	}
	if !authority.workspaceLease.ExpiresAt.Time.Equal(authority.runLease.ExpiresAt.Time) {
		return authority, errStaleRunLeaseClaim
	}
	return authority, nil
}
