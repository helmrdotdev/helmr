package controlplane

import (
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type runLeaseClaimAuthority struct {
	mode                 runLeaseClaimMode
	restoreSource        runLeaseRestoreSource
	actor                db.Actor
	parentRun            db.Run
	childRun             db.Run
	run                  db.Run
	workspace            db.Workspace
	parentAttempt        db.RunAttempt
	childAttempt         db.RunAttempt
	attempt              db.RunAttempt
	workerGroup          db.WorkerGroup
	worker               db.WorkerInstance
	workerObservation    db.LockRunLeaseClaimObservationRow
	runtime              db.RuntimeInstance
	runLease             db.RunLease
	workspaceMount       db.WorkspaceMount
	workspaceLease       db.WorkspaceLease
	enclosingWait        db.RunWait
	parentEnclosingWait  db.RunWait
	handoffAncestors     []db.LockSameWorkspaceHandoffAncestorsRow
	runWait              db.RunWait
	checkpoint           db.RunCheckpoint
	sourceRunLease       db.RunLease
	sourceWorkspaceLease db.WorkspaceLease
	sourceRuntime        db.RuntimeInstance
}

func (s *Server) claimRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, []secret.DeliveryEnvelope, error) {
	secretLocators, err := s.db.GetRunLeaseSecretDeliveryLocators(ctx, db.GetRunLeaseSecretDeliveryLocatorsParams{
		ID:                    leaseID,
		LeaseSequence:         leaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, nil, staleRunLeaseClaim(err)
	}

	var authority runLeaseClaimAuthority
	var envelopes []secret.DeliveryEnvelope
	err = s.inTx(ctx, func(work *txWork) error {
		envelopes, err = secret.LockAttemptDelivery(
			ctx,
			work.q,
			secretLocators.RunID,
			secretLocators.AttemptNumber,
			secretLocators.WorkspaceID,
		)
		if err != nil {
			return err
		}
		locators, err := work.q.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
			ID:                    leaseID,
			LeaseSequence:         leaseSequence,
			WorkerGroupID:         worker.WorkerGroupID,
			WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:           worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if locators.EnvironmentID != secretLocators.EnvironmentID ||
			locators.RunID != secretLocators.RunID ||
			locators.WorkspaceID != secretLocators.WorkspaceID ||
			locators.AttemptNumber != secretLocators.AttemptNumber {
			return errStaleRunLeaseClaim
		}
		switch {
		case locators.RunWaitID.Valid && locators.ResumeChildRunID.Valid:
			authority, err = claimSameWorkspaceParentResumeRunLeaseInTx(
				ctx, work.q, worker, leaseID, leaseSequence, locators,
			)
		case locators.RunWaitID.Valid:
			authority, err = claimCheckpointRestoreRunLeaseInTx(
				ctx, work.q, worker, leaseID, leaseSequence, locators,
			)
		case locators.EnclosingWaitID.Valid:
			authority, err = claimSameWorkspaceChildRunLeaseInTx(
				ctx, work.q, worker, leaseID, leaseSequence, locators,
			)
		case locators.ActorID.Valid:
			authority, err = claimActorRunLeaseInTx(
				ctx, work.q, worker, leaseID, leaseSequence, locators,
			)
		default:
			authority, err = claimFreshTaskRunLeaseInTx(
				ctx, work.q, worker, leaseID, leaseSequence, locators,
			)
		}
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, nil, err
	}
	return authority, envelopes, nil
}

func claimFreshTaskRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	if locators.RunWaitID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority := runLeaseClaimAuthority{mode: runLeaseClaimFresh}
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
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
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
	if err := validateClaimWorkspace(authority.run, authority.workspace); err != nil {
		return runLeaseClaimAuthority{}, err
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
		authority.attempt.EntrypointKind != authority.run.EntrypointKind ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		authority.attempt.BaseWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if authority.runtime.RestoreCheckpointID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return markRunLeaseStartingInTx(ctx, q, worker, leaseID, leaseSequence, authority)
}

func claimActorRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	if locators.RunWaitID.Valid ||
		!locators.ActorID.Valid ||
		!locators.ActorRunGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority := runLeaseClaimAuthority{mode: runLeaseClaimFresh}
	var err error
	authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
		ID:          locators.ActorID,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.actor.CurrentRunID != locators.RunID ||
		authority.actor.RunGeneration != locators.ActorRunGeneration.Int64 ||
		(authority.actor.State != "open" && authority.actor.State != "closing") {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

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
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID ||
		authority.run.EntrypointKind != "actor" ||
		authority.run.ActorID != authority.actor.ID ||
		authority.run.DeploymentDefinitionID != authority.actor.DeploymentDefinitionID ||
		authority.run.EntrypointDeclaredID != authority.actor.ActorDeclaredID {
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
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive ||
		authority.workspace.OwnerActorID != authority.actor.ID ||
		authority.workspace.OwnerRunID.Valid {
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
		authority.attempt.EntrypointKind != "actor" ||
		!authority.attempt.ActorStartInputSequence.Valid ||
		!authority.run.ActorStartInputSequence.Valid ||
		!authority.run.ActorStartInputHighWatermark.Valid ||
		authority.actor.CommittedInputSequence != authority.attempt.ActorStartInputSequence.Int64 ||
		authority.attempt.ActorStartInputSequence.Int64 < authority.run.ActorStartInputSequence.Int64 ||
		authority.run.ActorStartInputHighWatermark.Int64 < authority.attempt.ActorStartInputSequence.Int64 ||
		authority.run.ActorStartInputHighWatermark.Int64 >= authority.actor.NextInputSequence ||
		!authority.workspace.HeadVersionID.Valid ||
		authority.attempt.BaseWorkspaceVersionID != authority.workspace.HeadVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if authority.attempt.Number == 1 &&
		(authority.attempt.ActorStartInputSequence.Int64 != authority.run.ActorStartInputSequence.Int64 ||
			authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		authority.attempt.BaseWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if authority.runtime.RestoreCheckpointID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return markRunLeaseStartingInTx(ctx, q, worker, leaseID, leaseSequence, authority)
}

func claimSameWorkspaceChildRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	hasParentEnclosingWait := locators.ParentEnclosingWaitID.Valid ||
		locators.ParentEnclosingRunID.Valid ||
		locators.ParentEnclosingAttemptNumber != 0
	if locators.RunWaitID.Valid ||
		locators.HandoffResumeCheckpointID.Valid ||
		!locators.ParentRunID.Valid ||
		!locators.ParentOwnsLifecycle.Valid ||
		!locators.ParentOwnsLifecycle.Bool ||
		locators.ParentAttemptNumber <= 0 ||
		!locators.EnclosingWaitID.Valid ||
		!locators.EnclosingSuspendCheckpointID.Valid ||
		!locators.EnclosingResumeAttachID.Valid ||
		!locators.EnclosingBaseWorkspaceVersionID.Valid ||
		!locators.EnclosingRuntimeInstanceID.Valid ||
		!locators.EnclosingWorkspaceMountID.Valid ||
		!locators.EnclosingMountGeneration.Valid ||
		!locators.EnclosingOwnershipGeneration.Valid ||
		!locators.EnclosingParentWriterGeneration.Valid ||
		!locators.EnclosingChildWriterGeneration.Valid ||
		locators.EnclosingResumeWriterGeneration.Valid ||
		(hasParentEnclosingWait &&
			(!locators.ParentEnclosingWaitID.Valid ||
				!locators.ParentEnclosingRunID.Valid ||
				locators.ParentEnclosingAttemptNumber <= 0)) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority := runLeaseClaimAuthority{mode: runLeaseClaimAttachChild}
	var err error
	if locators.ParentActorID.Valid {
		if hasParentEnclosingWait {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID:          locators.ParentActorID,
			WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if !locators.ParentActorRunGeneration.Valid ||
			authority.actor.CurrentRunID != locators.ParentRunID ||
			authority.actor.RunGeneration != locators.ParentActorRunGeneration.Int64 ||
			(authority.actor.State != "open" && authority.actor.State != "closing") {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if locators.ParentActorRunGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.parentRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID:            locators.ParentRunID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.parentRun.Status != db.RunStatusWaiting ||
		authority.parentRun.CurrentAttemptNumber != locators.ParentAttemptNumber ||
		authority.parentRun.CurrentRunLeaseID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ParentActorID.Valid {
		if authority.parentRun.EntrypointKind != "actor" ||
			authority.parentRun.ActorID != authority.actor.ID ||
			authority.parentRun.DeploymentDefinitionID != authority.actor.DeploymentDefinitionID ||
			authority.parentRun.EntrypointDeclaredID != authority.actor.ActorDeclaredID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.parentRun.EntrypointKind != "task" || authority.parentRun.ActorID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

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
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID ||
		authority.run.EntrypointKind != "task" ||
		authority.run.ActorID.Valid ||
		authority.run.ParentRunID != authority.parentRun.ID ||
		!authority.run.ParentOwnsLifecycle.Valid ||
		!authority.run.ParentOwnsLifecycle.Bool ||
		authority.run.DeploymentID != authority.parentRun.DeploymentID ||
		authority.run.WorkspaceID != authority.parentRun.WorkspaceID ||
		authority.run.BaseWorkspaceVersionID != locators.EnclosingBaseWorkspaceVersionID {
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
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive ||
		authority.workspace.OwnershipGeneration != locators.EnclosingOwnershipGeneration.Int64 ||
		authority.workspace.WriterGeneration != locators.EnclosingChildWriterGeneration.Int64 {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ParentActorID.Valid {
		if authority.workspace.OwnerActorID != authority.actor.ID ||
			authority.workspace.OwnerRunID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if hasParentEnclosingWait {
		if authority.workspace.OwnerRunID.Valid == authority.workspace.OwnerActorID.Valid ||
			authority.workspace.OwnerRunID == authority.parentRun.ID ||
			authority.workspace.OwnerRunID == authority.run.ID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else {
		if authority.workspace.OwnerRunID != authority.parentRun.ID ||
			authority.workspace.OwnerActorID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	}

	authority.parentAttempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       authority.parentRun.ID,
		Number:      locators.ParentAttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.parentAttempt.TerminalAt.Valid ||
		!authority.parentAttempt.EntrypointEnteredAt.Valid ||
		authority.parentAttempt.EntrypointKind != authority.parentRun.EntrypointKind {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ParentActorID.Valid {
		if !authority.parentAttempt.ActorStartInputSequence.Valid ||
			!authority.parentRun.ActorStartInputSequence.Valid ||
			!authority.parentRun.ActorStartInputHighWatermark.Valid ||
			authority.parentAttempt.ActorStartInputSequence.Int64 < authority.parentRun.ActorStartInputSequence.Int64 ||
			authority.parentAttempt.ActorStartInputSequence.Int64 > authority.parentRun.ActorStartInputHighWatermark.Int64 ||
			authority.actor.CommittedInputSequence < authority.parentAttempt.ActorStartInputSequence.Int64 ||
			authority.actor.CommittedInputSequence >= authority.actor.NextInputSequence {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.parentAttempt.ActorStartInputSequence.Valid {
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
		authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.EntrypointKind != "task" ||
		authority.attempt.ActorStartInputSequence.Valid ||
		authority.attempt.BaseWorkspaceVersionID != locators.EnclosingBaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		locators.EnclosingBaseWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}

	if hasParentEnclosingWait {
		authority.enclosingWait, err = q.LockSameWorkspaceHandoffWait(ctx, db.LockSameWorkspaceHandoffWaitParams{
			ID:                  locators.ParentEnclosingWaitID,
			EnvironmentID:       locators.EnvironmentID,
			ParentRunID:         locators.ParentEnclosingRunID,
			ParentAttemptNumber: locators.ParentEnclosingAttemptNumber,
			WorkspaceID:         locators.WorkspaceID,
			ChildRunID:          authority.parentRun.ID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if err := validateActiveEnclosingWait(
			authority.enclosingWait,
			authority.parentRun,
			locators.EnclosingParentWriterGeneration.Int64,
			authority,
		); err != nil {
			return runLeaseClaimAuthority{}, err
		}
	}

	authority.runWait, err = q.LockSameWorkspaceHandoffWait(ctx, db.LockSameWorkspaceHandoffWaitParams{
		ID:                  locators.EnclosingWaitID,
		EnvironmentID:       locators.EnvironmentID,
		ParentRunID:         authority.parentRun.ID,
		ParentAttemptNumber: authority.parentAttempt.Number,
		WorkspaceID:         locators.WorkspaceID,
		ChildRunID:          authority.run.ID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateSameWorkspaceChildWait(locators, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.checkpoint, err = q.LockRestorableRunCheckpoint(ctx, db.LockRestorableRunCheckpointParams{
		ID:            authority.runWait.SuspendCheckpointID,
		RunID:         authority.parentRun.ID,
		AttemptNumber: authority.parentAttempt.Number,
		RunWaitID:     authority.runWait.ID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateSameWorkspaceChildCheckpoint(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	source, err := q.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: authority.checkpoint.SourceWorkspaceLeaseID,
		SourceRunLeaseID:       authority.checkpoint.SourceRunLeaseID,
		RunID:                  authority.parentRun.ID,
		AttemptNumber:          authority.parentAttempt.Number,
		WorkspaceID:            locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceWorkspaceLease = source.WorkspaceLease
	authority.sourceRuntime = source.RuntimeInstance
	if authority.sourceRuntime.ID != authority.runtime.ID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if err := validateCheckpointSource(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if authority.sourceWorkspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
		authority.sourceWorkspaceLease.MountFencingGeneration != authority.runWait.HandoffMountGeneration.Int64 ||
		authority.sourceWorkspaceLease.WriterGeneration != authority.runWait.ParentWriterGeneration.Int64 {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return markRunLeaseStartingInTx(ctx, q, worker, leaseID, leaseSequence, authority)
}

func claimSameWorkspaceParentResumeRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	hasEnclosingWait := hasEnclosingHandoffLocator(locators)
	if !locators.RunWaitID.Valid ||
		!locators.SuspendCheckpointID.Valid ||
		!locators.ResumeAttachID.Valid ||
		!locators.ResumeRequestVersion.Valid ||
		locators.ResumeRequestVersion.Int64 <= 0 ||
		!locators.ResumeChildRunID.Valid ||
		locators.ResumeChildAttemptNumber <= 0 ||
		!locators.HandoffResumeWorkspaceVersionID.Valid ||
		!locators.ResumeHandoffRuntimeInstanceID.Valid ||
		!locators.ResumeHandoffWorkspaceMountID.Valid ||
		!locators.ResumeHandoffMountGeneration.Valid ||
		!locators.ResumeHandoffOwnershipGeneration.Valid ||
		!locators.ResumeHandoffParentWriterGeneration.Valid ||
		!locators.ResumeHandoffChildWriterGeneration.Valid ||
		!locators.ResumeHandoffResumeWriterGeneration.Valid ||
		(hasEnclosingWait && !hasCompleteEnclosingHandoffLocator(locators)) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority := runLeaseClaimAuthority{}
	var err error
	if hasEnclosingWait {
		if locators.ActorID.Valid ||
			!locators.ParentRunID.Valid ||
			!locators.ParentOwnsLifecycle.Valid ||
			!locators.ParentOwnsLifecycle.Bool ||
			locators.ParentAttemptNumber <= 0 {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		authority.parentRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
			ID:            locators.ParentRunID,
			OrgID:         locators.OrgID,
			ProjectID:     locators.ProjectID,
			EnvironmentID: locators.EnvironmentID,
			WorkspaceID:   locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if authority.parentRun.Status != db.RunStatusWaiting ||
			authority.parentRun.CurrentAttemptNumber != locators.ParentAttemptNumber ||
			authority.parentRun.CurrentRunLeaseID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	}
	if locators.ActorID.Valid {
		authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID:          locators.ActorID,
			WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if !locators.ActorRunGeneration.Valid ||
			authority.actor.CurrentRunID != locators.RunID ||
			authority.actor.RunGeneration != locators.ActorRunGeneration.Int64 ||
			(authority.actor.State != "open" && authority.actor.State != "closing") {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if locators.ActorRunGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

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
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if hasEnclosingWait &&
		(authority.run.ParentRunID != authority.parentRun.ID ||
			!authority.run.ParentOwnsLifecycle.Valid ||
			!authority.run.ParentOwnsLifecycle.Bool ||
			authority.run.WorkspaceID != authority.parentRun.WorkspaceID ||
			authority.run.DeploymentID != authority.parentRun.DeploymentID) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ActorID.Valid {
		if authority.run.EntrypointKind != "actor" ||
			authority.run.ActorID != authority.actor.ID ||
			authority.run.DeploymentDefinitionID != authority.actor.DeploymentDefinitionID ||
			authority.run.EntrypointDeclaredID != authority.actor.ActorDeclaredID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.run.EntrypointKind != "task" || authority.run.ActorID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.childRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID:            locators.ResumeChildRunID,
		OrgID:         locators.OrgID,
		ProjectID:     locators.ProjectID,
		EnvironmentID: locators.EnvironmentID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.childRun.ParentRunID != authority.run.ID ||
		!authority.childRun.ParentOwnsLifecycle.Valid ||
		!authority.childRun.ParentOwnsLifecycle.Bool ||
		authority.childRun.WorkspaceID != authority.run.WorkspaceID ||
		authority.childRun.DeploymentID != authority.run.DeploymentID ||
		authority.childRun.EntrypointKind != "task" ||
		authority.childRun.ActorID.Valid ||
		authority.childRun.CurrentAttemptNumber != locators.ResumeChildAttemptNumber ||
		authority.childRun.CurrentRunLeaseID.Valid {
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
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive ||
		authority.workspace.OwnershipGeneration != locators.ResumeHandoffOwnershipGeneration.Int64 ||
		authority.workspace.WriterGeneration != locators.ResumeHandoffResumeWriterGeneration.Int64 {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if hasEnclosingWait {
		if authority.workspace.OwnershipGeneration != locators.EnclosingOwnershipGeneration.Int64 ||
			authority.workspace.OwnerRunID.Valid == authority.workspace.OwnerActorID.Valid ||
			authority.workspace.OwnerRunID == authority.run.ID ||
			authority.workspace.OwnerRunID == authority.childRun.ID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if locators.ActorID.Valid {
		if authority.workspace.OwnerActorID != authority.actor.ID ||
			authority.workspace.OwnerRunID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.workspace.OwnerRunID != authority.run.ID ||
		authority.workspace.OwnerActorID.Valid {
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
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ActorID.Valid {
		if !authority.attempt.ActorStartInputSequence.Valid ||
			!authority.run.ActorStartInputSequence.Valid ||
			!authority.run.ActorStartInputHighWatermark.Valid ||
			authority.attempt.ActorStartInputSequence.Int64 < authority.run.ActorStartInputSequence.Int64 ||
			authority.attempt.ActorStartInputSequence.Int64 > authority.run.ActorStartInputHighWatermark.Int64 ||
			authority.actor.CommittedInputSequence < authority.attempt.ActorStartInputSequence.Int64 ||
			authority.actor.CommittedInputSequence >= authority.actor.NextInputSequence {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.attempt.ActorStartInputSequence.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.childAttempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID:       authority.childRun.ID,
		Number:      locators.ResumeChildAttemptNumber,
		WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if !authority.childAttempt.TerminalAt.Valid ||
		!authority.childAttempt.TerminalOutcome.Valid ||
		authority.childAttempt.EntrypointKind != "task" {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		locators.HandoffResumeWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}

	if hasEnclosingWait {
		authority.enclosingWait, err = q.LockSameWorkspaceHandoffWait(ctx, db.LockSameWorkspaceHandoffWaitParams{
			ID:                  locators.EnclosingWaitID,
			EnvironmentID:       locators.EnvironmentID,
			ParentRunID:         authority.parentRun.ID,
			ParentAttemptNumber: authority.parentRun.CurrentAttemptNumber,
			WorkspaceID:         locators.WorkspaceID,
			ChildRunID:          authority.run.ID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if err := validateActiveEnclosingWait(
			authority.enclosingWait,
			authority.run,
			locators.ResumeHandoffResumeWriterGeneration.Int64,
			authority,
		); err != nil {
			return runLeaseClaimAuthority{}, err
		}
	}

	authority.runWait, err = q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
		ID:                locators.RunWaitID,
		EnvironmentID:     locators.EnvironmentID,
		RunID:             locators.RunID,
		AttemptNumber:     locators.AttemptNumber,
		WorkspaceID:       locators.WorkspaceID,
		CurrentRunLeaseID: leaseID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	checkpointID, checkpointKind, err := validateSameWorkspaceParentResumeWait(locators, authority)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	retained := authority.runtime.ID == authority.runWait.HandoffRuntimeInstanceID &&
		authority.workspaceMount.ID == authority.runWait.HandoffWorkspaceMountID
	switch authority.runWait.ConditionState {
	case db.WaitStateCompleted:
		if retained {
			authority.mode = runLeaseClaimAttachParent
		} else {
			authority.mode = runLeaseClaimRestore
			authority.restoreSource = runLeaseRestoreRecreated
		}
	case db.WaitStateFailed, db.WaitStateCancelled:
		if retained {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		authority.mode = runLeaseClaimRestore
		authority.restoreSource = runLeaseRestoreRecreated
	default:
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if hasEnclosingWait && !retained {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority.checkpoint, err = q.LockReadyRunCheckpoint(ctx, db.LockReadyRunCheckpointParams{
		ID:            checkpointID,
		Kind:          checkpointKind,
		RunID:         authority.run.ID,
		AttemptNumber: authority.attempt.Number,
		RunWaitID:     authority.runWait.ID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateSameWorkspaceParentResumeCheckpoint(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	source, err := q.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: authority.checkpoint.SourceWorkspaceLeaseID,
		SourceRunLeaseID:       authority.checkpoint.SourceRunLeaseID,
		RunID:                  authority.run.ID,
		AttemptNumber:          authority.attempt.Number,
		WorkspaceID:            locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceWorkspaceLease = source.WorkspaceLease
	authority.sourceRuntime = source.RuntimeInstance
	if err := validateCheckpointSource(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if retained {
		if authority.sourceRuntime.ID != authority.runtime.ID ||
			authority.sourceWorkspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
			authority.sourceWorkspaceLease.MountFencingGeneration != authority.runWait.HandoffMountGeneration.Int64 ||
			authority.sourceWorkspaceLease.WriterGeneration != authority.runWait.ParentWriterGeneration.Int64 {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.runtime.RestoreCheckpointID != authority.checkpoint.ID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return markRunLeaseStartingInTx(ctx, q, worker, leaseID, leaseSequence, authority)
}

func claimCheckpointRestoreRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	hasEnclosingWait := hasEnclosingHandoffLocator(locators)
	if !locators.RunWaitID.Valid ||
		!locators.SuspendCheckpointID.Valid ||
		locators.HandoffResumeCheckpointID.Valid ||
		!locators.ResumeAttachID.Valid ||
		!locators.ResumeRequestVersion.Valid ||
		locators.ResumeRequestVersion.Int64 <= 0 ||
		!locators.CheckpointPrivateWorkspaceVersionID.Valid ||
		(hasEnclosingWait && !hasCompleteEnclosingHandoffLocator(locators)) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority := runLeaseClaimAuthority{mode: runLeaseClaimRestore}
	var err error
	if hasEnclosingWait {
		if locators.ActorID.Valid ||
			!locators.ParentRunID.Valid ||
			!locators.ParentOwnsLifecycle.Valid ||
			!locators.ParentOwnsLifecycle.Bool ||
			locators.ParentAttemptNumber <= 0 {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		authority.parentRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
			ID:            locators.ParentRunID,
			OrgID:         locators.OrgID,
			ProjectID:     locators.ProjectID,
			EnvironmentID: locators.EnvironmentID,
			WorkspaceID:   locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if authority.parentRun.Status != db.RunStatusWaiting ||
			authority.parentRun.CurrentAttemptNumber != locators.ParentAttemptNumber ||
			authority.parentRun.CurrentRunLeaseID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	}
	if locators.ActorID.Valid {
		authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID:          locators.ActorID,
			WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if !locators.ActorRunGeneration.Valid ||
			authority.actor.CurrentRunID != locators.RunID ||
			authority.actor.RunGeneration != locators.ActorRunGeneration.Int64 ||
			(authority.actor.State != "open" && authority.actor.State != "closing") {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if locators.ActorRunGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

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
	if authority.run.Status != db.RunStatusQueued ||
		authority.run.CurrentAttemptNumber != locators.AttemptNumber ||
		authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if hasEnclosingWait &&
		(authority.run.ParentRunID != authority.parentRun.ID ||
			!authority.run.ParentOwnsLifecycle.Valid ||
			!authority.run.ParentOwnsLifecycle.Bool ||
			authority.run.WorkspaceID != authority.parentRun.WorkspaceID ||
			authority.run.DeploymentID != authority.parentRun.DeploymentID) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ActorID.Valid {
		if authority.run.EntrypointKind != "actor" ||
			authority.run.ActorID != authority.actor.ID ||
			authority.run.DeploymentDefinitionID != authority.actor.DeploymentDefinitionID ||
			authority.run.EntrypointDeclaredID != authority.actor.ActorDeclaredID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.run.EntrypointKind != "task" || authority.run.ActorID.Valid {
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
	if hasEnclosingWait {
		if authority.workspace.State != db.WorkspaceStateActive ||
			authority.workspace.DesiredState != db.WorkspaceDesiredStateActive ||
			authority.workspace.OwnershipGeneration != locators.EnclosingOwnershipGeneration.Int64 ||
			authority.workspace.WriterGeneration != locators.EnclosingChildWriterGeneration.Int64 ||
			authority.workspace.OwnerRunID.Valid == authority.workspace.OwnerActorID.Valid ||
			authority.workspace.OwnerRunID == authority.run.ID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if locators.ActorID.Valid {
		if authority.workspace.State != db.WorkspaceStateActive ||
			authority.workspace.DesiredState != db.WorkspaceDesiredStateActive ||
			authority.workspace.OwnerActorID != authority.actor.ID ||
			authority.workspace.OwnerRunID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if err := validateClaimWorkspace(authority.run, authority.workspace); err != nil {
		return runLeaseClaimAuthority{}, err
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
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ActorID.Valid {
		if !authority.attempt.ActorStartInputSequence.Valid ||
			!authority.run.ActorStartInputSequence.Valid ||
			!authority.run.ActorStartInputHighWatermark.Valid ||
			authority.attempt.ActorStartInputSequence.Int64 < authority.run.ActorStartInputSequence.Int64 ||
			authority.attempt.ActorStartInputSequence.Int64 > authority.run.ActorStartInputHighWatermark.Int64 ||
			authority.actor.CommittedInputSequence < authority.attempt.ActorStartInputSequence.Int64 ||
			authority.actor.CommittedInputSequence >= authority.actor.NextInputSequence {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.attempt.ActorStartInputSequence.Valid ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		locators.CheckpointPrivateWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}

	if hasEnclosingWait {
		authority.enclosingWait, err = q.LockSameWorkspaceHandoffWait(ctx, db.LockSameWorkspaceHandoffWaitParams{
			ID:                  locators.EnclosingWaitID,
			EnvironmentID:       locators.EnvironmentID,
			ParentRunID:         authority.parentRun.ID,
			ParentAttemptNumber: authority.parentRun.CurrentAttemptNumber,
			WorkspaceID:         locators.WorkspaceID,
			ChildRunID:          authority.run.ID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if err := validateActiveEnclosingWait(
			authority.enclosingWait,
			authority.run,
			authority.workspace.WriterGeneration,
			authority,
		); err != nil {
			return runLeaseClaimAuthority{}, err
		}
	}

	authority.runWait, err = q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
		ID:                locators.RunWaitID,
		EnvironmentID:     locators.EnvironmentID,
		RunID:             locators.RunID,
		AttemptNumber:     locators.AttemptNumber,
		WorkspaceID:       locators.WorkspaceID,
		CurrentRunLeaseID: leaseID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateCheckpointRestoreWait(locators, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.checkpoint, err = q.LockRestorableRunCheckpoint(ctx, db.LockRestorableRunCheckpointParams{
		ID:            authority.runWait.SuspendCheckpointID,
		RunID:         locators.RunID,
		AttemptNumber: locators.AttemptNumber,
		RunWaitID:     authority.runWait.ID,
		WorkspaceID:   locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateCheckpointRestore(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	source, err := q.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: authority.checkpoint.SourceWorkspaceLeaseID,
		SourceRunLeaseID:       authority.checkpoint.SourceRunLeaseID,
		RunID:                  locators.RunID,
		AttemptNumber:          locators.AttemptNumber,
		WorkspaceID:            locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceWorkspaceLease = source.WorkspaceLease
	authority.sourceRuntime = source.RuntimeInstance
	if err := validateCheckpointSource(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if hasEnclosingWait &&
		(authority.sourceRuntime.ID != authority.runtime.ID ||
			authority.sourceWorkspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
			authority.sourceWorkspaceLease.MountFencingGeneration != authority.enclosingWait.HandoffMountGeneration.Int64) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if hasEnclosingWait {
		authority.restoreSource = runLeaseRestoreRetained
	} else {
		if authority.runtime.RestoreCheckpointID != authority.checkpoint.ID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		authority.restoreSource = runLeaseRestoreRecreated
	}
	return markRunLeaseStartingInTx(ctx, q, worker, leaseID, leaseSequence, authority)
}

func claimRunLeasePhysicalInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
	expectedBaseVersionID pgtype.UUID,
	authority runLeaseClaimAuthority,
) (runLeaseClaimAuthority, error) {
	var err error
	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID:       worker.WorkerGroupID,
		RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if (authority.workerGroup.State != db.WorkerGroupStateActive &&
		authority.workerGroup.State != db.WorkerGroupStateDraining) ||
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
	authority.workerObservation, err = q.LockRunLeaseClaimObservation(
		ctx,
		db.LockRunLeaseClaimObservationParams{
			WorkerGroupID:    worker.WorkerGroupID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch:      worker.WorkerEpoch,
		},
	)
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
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

	authority.runLease, err = q.LockRunLeaseClaimLease(ctx, db.LockRunLeaseClaimLeaseParams{
		ID:            leaseID,
		RunID:         locators.RunID,
		WorkspaceID:   locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber,
		LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
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
	if err := validateClaimWorkspaceAuthority(authority, expectedBaseVersionID); err != nil {
		return runLeaseClaimAuthority{}, err
	}

	return authority, nil
}

func markRunLeaseStartingInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	authority runLeaseClaimAuthority,
) (runLeaseClaimAuthority, error) {
	if authority.runLease.State != db.RunLeaseStateAssigned {
		return authority, nil
	}
	runLease, err := q.MarkRunLeaseStarting(ctx, db.MarkRunLeaseStartingParams{
		ID:                    leaseID,
		LeaseSequence:         leaseSequence,
		WorkerGroupID:         worker.WorkerGroupID,
		WorkerInstanceID:      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:           worker.WorkerEpoch,
		WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.runLease = runLease
	return authority, nil
}

func validateClaimWorkspace(run db.Run, workspace db.Workspace) error {
	if workspace.State != db.WorkspaceStateActive ||
		workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return errStaleRunLeaseClaim
	}
	if run.EntrypointKind != "task" ||
		run.ActorID.Valid ||
		workspace.OwnerRunID != run.ID ||
		workspace.OwnerActorID.Valid {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimWorker(authenticated workerActor, worker db.WorkerInstance) error {
	if !worker.CurrentEpoch.Valid ||
		worker.CurrentEpoch.Int64 != authenticated.WorkerEpoch ||
		worker.ClaimVersion != authenticated.ClaimVersion ||
		worker.ProtocolVersion != authenticated.ProtocolVersion ||
		!worker.SupportsRun ||
		(worker.State != db.WorkerInstanceStateActive && worker.State != db.WorkerInstanceStateDraining) {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimPhysicalAuthority(worker workerActor, authority runLeaseClaimAuthority) error {
	lease := authority.runLease
	runtime := authority.runtime
	if lease.WorkerGroupID != worker.WorkerGroupID ||
		lease.WorkerInstanceID != pgvalue.UUID(worker.WorkerInstanceID) ||
		lease.WorkerEpoch != worker.WorkerEpoch ||
		lease.WorkerProtocolVersion != worker.ProtocolVersion ||
		lease.RuntimeInstanceID != runtime.ID ||
		lease.RuntimeIdentityID != runtime.RuntimeIdentityID {
		return errStaleRunLeaseClaim
	}
	if lease.State == db.RunLeaseStateAssigned && authority.worker.State != db.WorkerInstanceStateActive {
		return errStaleRunLeaseClaim
	}
	if lease.State == db.RunLeaseStateAssigned &&
		(authority.workerGroup.State != db.WorkerGroupStateActive ||
			!authority.workerGroup.AllowsRun) {
		return errStaleRunLeaseClaim
	}
	if lease.State == db.RunLeaseStateAssigned && !authority.workerObservation.RunReady {
		return errStaleRunLeaseClaim
	}
	if runtime.DesiredState != db.RuntimeDesiredStateReady ||
		runtime.ObservedState != db.RuntimeObservedStateReady ||
		runtime.ObservedDesiredVersion != runtime.DesiredVersion ||
		runtime.ReclaimedAt.Valid ||
		runtime.ProgramDeploymentID != authority.run.DeploymentID ||
		runtime.DeploymentDefinitionID != authority.workspace.DeploymentDefinitionID ||
		runtime.ReservedRunID.Valid ||
		runtime.ReservedAttemptNumber.Valid ||
		runtime.ReservedProcessID.Valid ||
		runtime.ReservedWorkspaceVersionID.Valid ||
		runtime.ReservationExpiresAt.Valid {
		return errStaleRunLeaseClaim
	}
	if !authority.worker.RuntimeIdentityID.Valid ||
		authority.worker.RuntimeIdentityID.String != runtime.RuntimeIdentityID ||
		runtime.ReservedCPUMillis != lease.RequestedCPUMillis ||
		runtime.ReservedMemoryBytes != lease.RequestedMemoryBytes ||
		runtime.ReservedGuestEphemeralDiskBytes != lease.RequestedGuestEphemeralDiskBytes ||
		runtime.ReservedExecutionSlots != lease.RequestedExecutionSlots ||
		authority.worker.PerVMCPUMillis != lease.RequestedCPUMillis ||
		authority.worker.PerVMMemoryBytes != lease.RequestedMemoryBytes ||
		authority.worker.PerVMGuestEphemeralDiskBytes != lease.RequestedGuestEphemeralDiskBytes {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimWorkspaceAuthority(
	authority runLeaseClaimAuthority,
	expectedBaseVersionID pgtype.UUID,
) error {
	if err := validateRunLeaseWorkspaceAuthority(authority); err != nil {
		return err
	}
	if authority.workspaceLease.BaseVersionID != expectedBaseVersionID {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateRunLeaseWorkspaceAuthority(authority runLeaseClaimAuthority) error {
	mount := authority.workspaceMount
	lease := authority.workspaceLease
	if mount.State != db.WorkspaceMountStateMounted ||
		lease.State != db.WorkspaceLeaseStateActive ||
		lease.OwnerRunLeaseID != authority.runLease.ID ||
		lease.OwnerProcessID.Valid ||
		lease.BaseVersionID != mount.MaterializedVersionID ||
		lease.MountFencingGeneration != mount.FencingGeneration ||
		lease.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		lease.WriterGeneration != authority.workspace.WriterGeneration {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateCheckpointRestoreWait(
	locators db.GetRunLeaseClaimLocatorsRow,
	authority runLeaseClaimAuthority,
) error {
	wait := authority.runWait
	if wait.SuspensionState != db.RunWaitStateResuming ||
		wait.ConditionState == db.WaitStatePending ||
		!wait.PriorRunLeaseID.Valid ||
		wait.PriorRunLeaseID == authority.runLease.ID ||
		wait.SuspendCheckpointID != locators.SuspendCheckpointID ||
		wait.HandoffResumeCheckpointID.Valid ||
		wait.ResumeAttachID != locators.ResumeAttachID ||
		wait.ResumeRequestVersion != locators.ResumeRequestVersion.Int64 ||
		wait.ResumeRequestVersion <= wait.ResumeAckVersion ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		wait.BaseWorkspaceVersionID.Valid ||
		wait.BaseWorkspaceContentDigest.Valid ||
		wait.ChildResultVersionID.Valid ||
		wait.ResumeWorkspaceVersionID.Valid ||
		wait.HandoffRuntimeInstanceID.Valid ||
		wait.HandoffWorkspaceMountID.Valid ||
		wait.HandoffMountGeneration.Valid ||
		wait.OwnershipGeneration.Valid ||
		wait.ParentWriterGeneration.Valid ||
		wait.ChildWriterGeneration.Valid ||
		wait.ResumeWriterGeneration.Valid {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateCheckpointRestore(authority runLeaseClaimAuthority) error {
	checkpoint := authority.checkpoint
	if checkpoint.Kind != db.RunCheckpointKindSuspend ||
		checkpoint.State != db.RunCheckpointStateReady ||
		checkpoint.SourceRunLeaseID != authority.runWait.PriorRunLeaseID ||
		checkpoint.BaseWorkspaceVersionID != authority.attempt.BaseWorkspaceVersionID ||
		checkpoint.PrivateWorkspaceVersionID != authority.workspaceLease.BaseVersionID {
		return errStaleRunLeaseClaim
	}
	if authority.run.EntrypointKind == "task" {
		if checkpoint.ActorSpeculativeInputSequence.Valid {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	if !checkpoint.ActorSpeculativeInputSequence.Valid ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.actor.CommittedInputSequence ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.attempt.ActorStartInputSequence.Int64 ||
		checkpoint.ActorSpeculativeInputSequence.Int64 >= authority.actor.NextInputSequence {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateCheckpointSource(authority runLeaseClaimAuthority) error {
	sourceLease := authority.sourceRunLease
	sourceWorkspaceLease := authority.sourceWorkspaceLease
	sourceRuntime := authority.sourceRuntime
	currentLease := authority.runLease
	currentRuntime := authority.runtime
	if sourceLease.ID != authority.checkpoint.SourceRunLeaseID ||
		sourceLease.State != db.RunLeaseStateCheckpointed ||
		sourceWorkspaceLease.ID != authority.checkpoint.SourceWorkspaceLeaseID ||
		sourceWorkspaceLease.OwnerRunLeaseID != sourceLease.ID ||
		sourceWorkspaceLease.OwnerProcessID.Valid ||
		(sourceWorkspaceLease.State != db.WorkspaceLeaseStateReleased &&
			sourceWorkspaceLease.State != db.WorkspaceLeaseStateFenced) ||
		sourceWorkspaceLease.WorkspaceID != authority.checkpoint.WorkspaceID ||
		sourceWorkspaceLease.BaseVersionID != authority.checkpoint.BaseWorkspaceVersionID ||
		sourceWorkspaceLease.OwnershipGeneration != authority.workspace.OwnershipGeneration ||
		sourceWorkspaceLease.WriterGeneration >= authority.workspace.WriterGeneration ||
		sourceLease.RuntimeInstanceID != sourceRuntime.ID ||
		sourceLease.RuntimeIdentityID != sourceRuntime.RuntimeIdentityID ||
		sourceLease.RuntimeIdentityID != currentLease.RuntimeIdentityID ||
		sourceLease.RequestedCPUMillis != currentLease.RequestedCPUMillis ||
		sourceLease.RequestedMemoryBytes != currentLease.RequestedMemoryBytes ||
		sourceLease.RequestedGuestEphemeralDiskBytes != currentLease.RequestedGuestEphemeralDiskBytes ||
		sourceLease.RequestedExecutionSlots != currentLease.RequestedExecutionSlots ||
		sourceRuntime.RuntimeIdentityID != currentRuntime.RuntimeIdentityID ||
		sourceRuntime.RuntimeSubstrateID != currentRuntime.RuntimeSubstrateID ||
		sourceRuntime.DeploymentDefinitionID != currentRuntime.DeploymentDefinitionID ||
		sourceRuntime.WorkspaceID != currentRuntime.WorkspaceID ||
		sourceRuntime.ProgramDeploymentID != currentRuntime.ProgramDeploymentID ||
		sourceRuntime.ReservedCPUMillis != currentRuntime.ReservedCPUMillis ||
		sourceRuntime.ReservedMemoryBytes != currentRuntime.ReservedMemoryBytes ||
		sourceRuntime.ReservedGuestEphemeralDiskBytes != currentRuntime.ReservedGuestEphemeralDiskBytes ||
		sourceRuntime.ReservedExecutionSlots != currentRuntime.ReservedExecutionSlots {
		return errStaleRunLeaseClaim
	}
	return nil
}

func hasEnclosingHandoffLocator(locators db.GetRunLeaseClaimLocatorsRow) bool {
	return locators.EnclosingWaitID.Valid ||
		locators.EnclosingSuspendCheckpointID.Valid ||
		locators.EnclosingResumeAttachID.Valid ||
		locators.EnclosingBaseWorkspaceVersionID.Valid ||
		locators.EnclosingRuntimeInstanceID.Valid ||
		locators.EnclosingWorkspaceMountID.Valid ||
		locators.EnclosingMountGeneration.Valid ||
		locators.EnclosingOwnershipGeneration.Valid ||
		locators.EnclosingParentWriterGeneration.Valid ||
		locators.EnclosingChildWriterGeneration.Valid ||
		locators.EnclosingResumeWriterGeneration.Valid
}

func hasCompleteEnclosingHandoffLocator(locators db.GetRunLeaseClaimLocatorsRow) bool {
	return locators.EnclosingWaitID.Valid &&
		locators.EnclosingSuspendCheckpointID.Valid &&
		locators.EnclosingResumeAttachID.Valid &&
		locators.EnclosingBaseWorkspaceVersionID.Valid &&
		locators.EnclosingRuntimeInstanceID.Valid &&
		locators.EnclosingWorkspaceMountID.Valid &&
		locators.EnclosingMountGeneration.Valid &&
		locators.EnclosingOwnershipGeneration.Valid &&
		locators.EnclosingParentWriterGeneration.Valid &&
		locators.EnclosingChildWriterGeneration.Valid &&
		!locators.EnclosingResumeWriterGeneration.Valid
}

func validateActiveEnclosingWait(
	wait db.RunWait,
	child db.Run,
	expectedWriterGeneration int64,
	authority runLeaseClaimAuthority,
) error {
	if wait.Kind != db.WaitKindChild ||
		wait.ConditionState != db.WaitStatePending ||
		wait.SuspensionState != db.RunWaitStateParked ||
		wait.CurrentRunLeaseID.Valid ||
		!wait.PriorRunLeaseID.Valid ||
		!wait.ChildRunID.Valid ||
		wait.ChildRunID != child.ID ||
		!wait.ChildParentOwned.Valid ||
		!wait.ChildParentOwned.Bool ||
		!wait.ChildTargetDeclaredID.Valid ||
		wait.ChildTargetDeclaredID.String != child.EntrypointDeclaredID ||
		!wait.SuspendCheckpointID.Valid ||
		wait.HandoffResumeCheckpointID.Valid ||
		!wait.ResumeAttachID.Valid ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		wait.ResumeRequestVersion != wait.ResumeAckVersion ||
		!wait.BaseWorkspaceVersionID.Valid ||
		wait.BaseWorkspaceVersionID != child.BaseWorkspaceVersionID ||
		!wait.BaseWorkspaceContentDigest.Valid ||
		wait.ChildResultVersionID.Valid ||
		wait.ResumeWorkspaceVersionID.Valid ||
		wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
		wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
		!wait.HandoffMountGeneration.Valid ||
		!wait.OwnershipGeneration.Valid ||
		wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
		!wait.ParentWriterGeneration.Valid ||
		!wait.ChildWriterGeneration.Valid ||
		wait.ResumeWriterGeneration.Valid ||
		wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64 ||
		wait.ChildWriterGeneration.Int64 != expectedWriterGeneration {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateSameWorkspaceChildWait(
	locators db.GetRunLeaseClaimLocatorsRow,
	authority runLeaseClaimAuthority,
) error {
	wait := authority.runWait
	if wait.Kind != db.WaitKindChild ||
		wait.ConditionState != db.WaitStatePending ||
		wait.SuspensionState != db.RunWaitStateParked ||
		wait.CurrentRunLeaseID.Valid ||
		!wait.PriorRunLeaseID.Valid ||
		!wait.ChildRunID.Valid ||
		wait.ChildRunID != authority.run.ID ||
		!wait.ChildParentOwned.Valid ||
		!wait.ChildParentOwned.Bool ||
		!wait.ChildTargetDeclaredID.Valid ||
		wait.ChildTargetDeclaredID.String != authority.run.EntrypointDeclaredID ||
		wait.SuspendCheckpointID != locators.EnclosingSuspendCheckpointID ||
		wait.HandoffResumeCheckpointID.Valid ||
		wait.ResumeAttachID != locators.EnclosingResumeAttachID ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		wait.ResumeRequestVersion != wait.ResumeAckVersion ||
		wait.BaseWorkspaceVersionID != locators.EnclosingBaseWorkspaceVersionID ||
		!wait.BaseWorkspaceContentDigest.Valid ||
		wait.ChildResultVersionID.Valid ||
		wait.ResumeWorkspaceVersionID.Valid ||
		wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
		wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
		!wait.HandoffMountGeneration.Valid ||
		wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
		!wait.ParentWriterGeneration.Valid ||
		!wait.ChildWriterGeneration.Valid ||
		wait.ResumeWriterGeneration.Valid ||
		wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64 ||
		wait.ChildWriterGeneration.Int64 != authority.workspace.WriterGeneration ||
		wait.ChildWriterGeneration.Int64 != authority.workspaceLease.WriterGeneration {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateSameWorkspaceChildCheckpoint(authority runLeaseClaimAuthority) error {
	checkpoint := authority.checkpoint
	if checkpoint.Kind != db.RunCheckpointKindSuspend ||
		checkpoint.State != db.RunCheckpointStateReady ||
		checkpoint.SourceRunLeaseID != authority.runWait.PriorRunLeaseID ||
		checkpoint.BaseWorkspaceVersionID != authority.parentAttempt.BaseWorkspaceVersionID ||
		checkpoint.PrivateWorkspaceVersionID != authority.runWait.BaseWorkspaceVersionID {
		return errStaleRunLeaseClaim
	}
	if authority.parentRun.EntrypointKind == "task" {
		if checkpoint.ActorSpeculativeInputSequence.Valid {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	if !checkpoint.ActorSpeculativeInputSequence.Valid ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.actor.CommittedInputSequence ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.parentAttempt.ActorStartInputSequence.Int64 ||
		checkpoint.ActorSpeculativeInputSequence.Int64 >= authority.actor.NextInputSequence {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateSameWorkspaceParentResumeWait(
	locators db.GetRunLeaseClaimLocatorsRow,
	authority runLeaseClaimAuthority,
) (pgtype.UUID, db.RunCheckpointKind, error) {
	wait := authority.runWait
	if wait.Kind != db.WaitKindChild ||
		wait.SuspensionState != db.RunWaitStateResuming ||
		!wait.PriorRunLeaseID.Valid ||
		!wait.ChildRunID.Valid ||
		wait.ChildRunID != authority.childRun.ID ||
		!wait.ChildParentOwned.Valid ||
		!wait.ChildParentOwned.Bool ||
		!wait.ChildTargetDeclaredID.Valid ||
		wait.ChildTargetDeclaredID.String != authority.childRun.EntrypointDeclaredID ||
		wait.SuspendCheckpointID != locators.SuspendCheckpointID ||
		wait.ResumeAttachID != locators.ResumeAttachID ||
		wait.ResumeRequestVersion != locators.ResumeRequestVersion.Int64 ||
		wait.ResumeRequestVersion <= wait.ResumeAckVersion ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		!wait.BaseWorkspaceVersionID.Valid ||
		!wait.BaseWorkspaceContentDigest.Valid ||
		authority.childAttempt.BaseWorkspaceVersionID != wait.BaseWorkspaceVersionID ||
		wait.ResumeWorkspaceVersionID != locators.HandoffResumeWorkspaceVersionID ||
		!wait.HandoffMountGeneration.Valid ||
		wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
		wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64 ||
		wait.ChildWriterGeneration.Int64 >= wait.ResumeWriterGeneration.Int64 ||
		wait.ResumeWriterGeneration.Int64 != authority.workspace.WriterGeneration ||
		wait.ResumeWriterGeneration.Int64 != authority.workspaceLease.WriterGeneration {
		return pgtype.UUID{}, "", errStaleRunLeaseClaim
	}

	switch wait.ConditionState {
	case db.WaitStateCompleted:
		if authority.childRun.Status != db.RunStatusSucceeded ||
			authority.childAttempt.TerminalOutcome.String != "succeeded" ||
			!wait.HandoffResumeCheckpointID.Valid ||
			wait.HandoffResumeCheckpointID != locators.HandoffResumeCheckpointID ||
			!wait.ChildResultVersionID.Valid ||
			wait.ChildResultVersionID != wait.ResumeWorkspaceVersionID {
			return pgtype.UUID{}, "", errStaleRunLeaseClaim
		}
		return wait.HandoffResumeCheckpointID, db.RunCheckpointKindHandoffResume, nil
	case db.WaitStateFailed, db.WaitStateCancelled:
		if authority.childRun.Status == db.RunStatusSucceeded ||
			authority.childRun.Status == db.RunStatusQueued ||
			authority.childRun.Status == db.RunStatusRunning ||
			authority.childRun.Status == db.RunStatusWaiting ||
			authority.childRun.Status == db.RunStatusRetryDelayed ||
			authority.childRun.Status == db.RunStatusCancelRequested ||
			authority.childAttempt.TerminalOutcome.String == "succeeded" ||
			wait.HandoffResumeCheckpointID.Valid ||
			wait.ChildResultVersionID.Valid ||
			wait.ResumeWorkspaceVersionID != wait.BaseWorkspaceVersionID {
			return pgtype.UUID{}, "", errStaleRunLeaseClaim
		}
		return wait.SuspendCheckpointID, db.RunCheckpointKindSuspend, nil
	default:
		return pgtype.UUID{}, "", errStaleRunLeaseClaim
	}
}

func validateSameWorkspaceParentResumeCheckpoint(authority runLeaseClaimAuthority) error {
	checkpoint := authority.checkpoint
	expectedKind := db.RunCheckpointKindSuspend
	if authority.runWait.ConditionState == db.WaitStateCompleted {
		expectedKind = db.RunCheckpointKindHandoffResume
	}
	if checkpoint.Kind != expectedKind ||
		checkpoint.State != db.RunCheckpointStateReady ||
		checkpoint.SourceRunLeaseID != authority.runWait.PriorRunLeaseID ||
		checkpoint.BaseWorkspaceVersionID != authority.attempt.BaseWorkspaceVersionID ||
		checkpoint.PrivateWorkspaceVersionID != authority.runWait.ResumeWorkspaceVersionID {
		return errStaleRunLeaseClaim
	}
	if authority.run.EntrypointKind == "task" {
		if checkpoint.ActorSpeculativeInputSequence.Valid {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	if !checkpoint.ActorSpeculativeInputSequence.Valid ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.actor.CommittedInputSequence ||
		checkpoint.ActorSpeculativeInputSequence.Int64 < authority.attempt.ActorStartInputSequence.Int64 ||
		checkpoint.ActorSpeculativeInputSequence.Int64 >= authority.actor.NextInputSequence {
		return errStaleRunLeaseClaim
	}
	return nil
}

func staleRunLeaseClaim(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errStaleRunLeaseClaim
	}
	return err
}
