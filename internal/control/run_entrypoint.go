package control

import (
	"context"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func enterRunEntrypoint(
	ctx context.Context,
	store db.Querier,
	txb TxBeginner,
	worker workerActor,
	leaseID pgtype.UUID,
	request api.WorkerRunEntrypointRequest,
) error {
	return inTxWith(ctx, store, txb, func(work *txWork) error {
		locators, err := work.q.GetRunEntrypointLocators(ctx, db.GetRunEntrypointLocatorsParams{
			ID:                    leaseID,
			LeaseSequence:         request.Lease.LeaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		authority, err := lockRunEntrypointAuthority(
			ctx,
			work.q,
			worker,
			leaseID,
			request.Lease.LeaseSequence,
			locators,
		)
		if err != nil {
			return err
		}
		if authority.run.EntrypointKind != request.EntrypointKind ||
			authority.run.EntrypointDeclaredID != request.EntrypointDeclaredID {
			return errStaleRunLeaseClaim
		}
		if authority.attempt.EntrypointEnteredAt.Valid {
			return nil
		}
		_, err = work.q.MarkRunEntrypointEntered(ctx, db.MarkRunEntrypointEnteredParams{
			RunID:       authority.run.ID,
			Number:      authority.attempt.Number,
			WorkspaceID: authority.workspace.ID,
		})
		return staleRunLeaseClaim(err)
	})
}

func lockRunEntrypointAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunEntrypointLocatorsRow,
) (runLeaseClaimAuthority, error) {
	var authority runLeaseClaimAuthority
	var err error
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID:            locators.RunID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.run.Status != db.RunStatusRunning ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID ||
		(authority.run.EntrypointKind == "task") == authority.run.ActorID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID:            locators.WorkspaceID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		RegionID:      locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       locators.RunID,
		Number:      locators.AttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID:       worker.WorkerGroupID,
		RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workerGroup.State != db.WorkerGroupStateActive ||
		!authority.workerGroup.AllowsRun ||
		authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID:               locators.RuntimeInstanceID,
		OrgID:            locators.OrgID,
		ProjectID:        locators.ProjectID,
		EnvironmentID:    locators.EnvironmentID,
		RegionID:         locators.RegionID,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.runLease, err = q.LockRunEntrypointLease(ctx, db.LockRunEntrypointLeaseParams{
		ID:            leaseID,
		RunID:         locators.RunID,
		WorkspaceID:   locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber,
		LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if !authority.runLease.StartedAt.Valid ||
		!authority.run.StartedAt.Valid ||
		!authority.run.ActiveStartedAt.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID:                locators.WorkspaceMountID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}

	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID:                locators.WorkspaceLeaseID,
		OrgID:             locators.OrgID,
		ProjectID:         locators.ProjectID,
		EnvironmentID:     locators.EnvironmentID,
		RegionID:          locators.RegionID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID:       locators.WorkspaceID,
		WorkspaceMountID:  locators.WorkspaceMountID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorkspaceAuthority(
		authority,
		authority.attempt.BaseWorkspaceVersionID,
	); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}
