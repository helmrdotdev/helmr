package control

import (
	"bytes"
	"context"
	"errors"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleRunLeaseClaim = errors.New("run lease claim is stale")

type runLeaseClaimAuthority struct {
	actor          db.Actor
	parentRun      db.Run
	childRun       db.Run
	run            db.Run
	workspace      db.Workspace
	parentAttempt  db.RunAttempt
	childAttempt   db.RunAttempt
	attempt        db.RunAttempt
	workerGroup    db.WorkerGroup
	worker         db.WorkerInstance
	networkSlot    db.WorkerNetworkSlot
	runtime        db.RuntimeInstance
	runLease       db.RunLease
	workspaceMount db.WorkspaceMount
	workspaceLease db.WorkspaceLease
	runWait        db.RunWait
	checkpoint     db.RunCheckpoint
	sourceRunLease db.RunLease
	sourceRuntime  db.RuntimeInstance
}

func (s *Server) claimFreshTaskRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
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

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimFreshTaskRunLeaseInTx(ctx, work.q, worker, leaseID, leaseSequence, locators)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
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

	return claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		authority.attempt.BaseWorkspaceVersionID,
		authority,
	)
}

func (s *Server) claimActorRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
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

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimActorRunLeaseInTx(ctx, work.q, worker, leaseID, leaseSequence, locators)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
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

	var authority runLeaseClaimAuthority
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

	return claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		authority.attempt.BaseWorkspaceVersionID,
		authority,
	)
}

func (s *Server) claimCheckpointRestoreRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
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

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimCheckpointRestoreRunLeaseInTx(
			ctx,
			work.q,
			worker,
			leaseID,
			leaseSequence,
			locators,
		)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func (s *Server) claimSameWorkspaceChildRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
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

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimSameWorkspaceChildRunLeaseInTx(
			ctx,
			work.q,
			worker,
			leaseID,
			leaseSequence,
			locators,
		)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func claimSameWorkspaceChildRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	if locators.RunWaitID.Valid ||
		locators.HandoffResumeCheckpointID.Valid ||
		!locators.ParentRunID.Valid ||
		!locators.ParentOwnsLifecycle.Valid ||
		!locators.ParentOwnsLifecycle.Bool ||
		locators.ParentAttemptNumber <= 0 ||
		!locators.HandoffParentRunWaitID.Valid ||
		!locators.HandoffSuspendCheckpointID.Valid ||
		!locators.HandoffResumeAttachID.Valid ||
		!locators.HandoffBaseWorkspaceVersionID.Valid ||
		!locators.HandoffRuntimeInstanceID.Valid ||
		!locators.HandoffWorkspaceMountID.Valid ||
		!locators.HandoffMountGeneration.Valid ||
		!locators.HandoffOwnershipGeneration.Valid ||
		!locators.HandoffParentWriterGeneration.Valid ||
		!locators.HandoffChildWriterGeneration.Valid ||
		!locators.HandoffResumeWriterGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	var authority runLeaseClaimAuthority
	var err error
	if locators.ParentActorID.Valid {
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
		authority.run.BaseWorkspaceVersionID != locators.HandoffBaseWorkspaceVersionID {
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
		authority.workspace.OwnershipGeneration != locators.HandoffOwnershipGeneration.Int64 ||
		authority.workspace.WriterGeneration != locators.HandoffChildWriterGeneration.Int64 {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if locators.ParentActorID.Valid {
		if authority.workspace.OwnerActorID != authority.actor.ID ||
			authority.workspace.OwnerRunID.Valid {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.workspace.OwnerRunID != authority.parentRun.ID ||
		authority.workspace.OwnerActorID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
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
		authority.attempt.BaseWorkspaceVersionID != locators.HandoffBaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	authority, err = claimRunLeasePhysicalInTx(
		ctx,
		q,
		worker,
		leaseID,
		leaseSequence,
		locators,
		locators.HandoffBaseWorkspaceVersionID,
		authority,
	)
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}

	authority.runWait, err = q.LockSameWorkspaceChildClaimWait(ctx, db.LockSameWorkspaceChildClaimWaitParams{
		ID:                  locators.HandoffParentRunWaitID,
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

	source, err := q.GetRunCheckpointSourceRuntime(ctx, db.GetRunCheckpointSourceRuntimeParams{
		SourceRunLeaseID: authority.checkpoint.SourceRunLeaseID,
		RunID:            authority.parentRun.ID,
		AttemptNumber:    authority.parentAttempt.Number,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceRuntime = source.RuntimeInstance
	if authority.sourceRuntime.ID != authority.runtime.ID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if err := validateCheckpointSourceRuntime(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func (s *Server) claimSameWorkspaceParentResumeRunLease(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
) (runLeaseClaimAuthority, error) {
	locators, err := s.db.GetRunLeaseClaimLocators(ctx, db.GetRunLeaseClaimLocatorsParams{
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

	var authority runLeaseClaimAuthority
	err = s.inTx(ctx, func(work *txWork) error {
		authority, err = claimSameWorkspaceParentResumeRunLeaseInTx(
			ctx,
			work.q,
			worker,
			leaseID,
			leaseSequence,
			locators,
		)
		return err
	})
	if err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func claimSameWorkspaceParentResumeRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
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
		!locators.ResumeHandoffResumeWriterGeneration.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	var authority runLeaseClaimAuthority
	var err error
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
	if locators.ActorID.Valid {
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

	source, err := q.GetRunCheckpointSourceRuntime(ctx, db.GetRunCheckpointSourceRuntimeParams{
		SourceRunLeaseID: authority.checkpoint.SourceRunLeaseID,
		RunID:            authority.run.ID,
		AttemptNumber:    authority.attempt.Number,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceRuntime = source.RuntimeInstance
	if authority.sourceRuntime.ID != authority.runtime.ID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if err := validateCheckpointSourceRuntime(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
}

func claimCheckpointRestoreRunLeaseInTx(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetRunLeaseClaimLocatorsRow,
) (runLeaseClaimAuthority, error) {
	if !locators.RunWaitID.Valid ||
		!locators.SuspendCheckpointID.Valid ||
		locators.HandoffResumeCheckpointID.Valid ||
		!locators.ResumeAttachID.Valid ||
		!locators.ResumeRequestVersion.Valid ||
		locators.ResumeRequestVersion.Int64 <= 0 ||
		!locators.CheckpointPrivateWorkspaceVersionID.Valid {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}

	var authority runLeaseClaimAuthority
	var err error
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
	if locators.ActorID.Valid {
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
	source, err := q.GetRunCheckpointSourceRuntime(ctx, db.GetRunCheckpointSourceRuntimeParams{
		SourceRunLeaseID: authority.checkpoint.SourceRunLeaseID,
		RunID:            locators.RunID,
		AttemptNumber:    locators.AttemptNumber,
		WorkspaceID:      locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceRuntime = source.RuntimeInstance
	if err := validateCheckpointSourceRuntime(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	return authority, nil
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

	authority.networkSlot, err = q.LockRunLeaseClaimNetworkSlot(ctx, db.LockRunLeaseClaimNetworkSlotParams{
		ID:                locators.NetworkSlotID,
		WorkerGroupID:     worker.WorkerGroupID,
		WorkerInstanceID:  pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:       worker.WorkerEpoch,
		Generation:        locators.NetworkSlotGeneration,
		RuntimeInstanceID: locators.RuntimeInstanceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.networkSlot.State != db.WorkerNetworkSlotStateBound {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
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

	if authority.runLease.State == db.RunLeaseStateAssigned {
		authority.runLease, err = q.MarkRunLeaseStarting(ctx, db.MarkRunLeaseStartingParams{
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
	}
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
		lease.NetworkSlotID != authority.networkSlot.ID ||
		lease.NetworkSlotGeneration != authority.networkSlot.Generation ||
		lease.RuntimeIdentityID != runtime.RuntimeIdentityID {
		return errStaleRunLeaseClaim
	}
	if lease.State == db.RunLeaseStateAssigned && authority.worker.State != db.WorkerInstanceStateActive {
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
		runtime.ReservedCpuMillis != lease.RequestedCpuMillis ||
		runtime.ReservedMemoryBytes != lease.RequestedMemoryBytes ||
		runtime.ReservedWorkloadDiskBytes != lease.RequestedWorkloadDiskBytes ||
		runtime.ReservedScratchBytes != lease.RequestedScratchBytes ||
		runtime.ReservedExecutionSlots != lease.RequestedExecutionSlots ||
		authority.worker.PerVmCpuMillis != lease.RequestedCpuMillis ||
		authority.worker.PerVmMemoryBytes != lease.RequestedMemoryBytes ||
		authority.worker.PerVmWorkloadDiskBytes != lease.RequestedWorkloadDiskBytes ||
		authority.worker.PerVmScratchBytes != lease.RequestedScratchBytes {
		return errStaleRunLeaseClaim
	}
	return nil
}

func validateClaimWorkspaceAuthority(
	authority runLeaseClaimAuthority,
	expectedBaseVersionID pgtype.UUID,
) error {
	mount := authority.workspaceMount
	lease := authority.workspaceLease
	if mount.State != db.WorkspaceMountStateMounted ||
		lease.State != db.WorkspaceLeaseStateActive ||
		lease.OwnerRunLeaseID != authority.runLease.ID ||
		lease.OwnerProcessID.Valid ||
		lease.BaseVersionID != expectedBaseVersionID ||
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

func validateCheckpointSourceRuntime(authority runLeaseClaimAuthority) error {
	sourceLease := authority.sourceRunLease
	sourceRuntime := authority.sourceRuntime
	currentLease := authority.runLease
	currentRuntime := authority.runtime
	if sourceLease.ID != authority.checkpoint.SourceRunLeaseID ||
		sourceLease.State != db.RunLeaseStateCheckpointed ||
		sourceLease.RuntimeInstanceID != sourceRuntime.ID ||
		sourceLease.RuntimeIdentityID != sourceRuntime.RuntimeIdentityID ||
		sourceLease.RuntimeIdentityID != currentLease.RuntimeIdentityID ||
		sourceLease.RequestedCpuMillis != currentLease.RequestedCpuMillis ||
		sourceLease.RequestedMemoryBytes != currentLease.RequestedMemoryBytes ||
		sourceLease.RequestedWorkloadDiskBytes != currentLease.RequestedWorkloadDiskBytes ||
		sourceLease.RequestedScratchBytes != currentLease.RequestedScratchBytes ||
		sourceLease.RequestedExecutionSlots != currentLease.RequestedExecutionSlots ||
		sourceRuntime.RuntimeIdentityID != currentRuntime.RuntimeIdentityID ||
		sourceRuntime.RuntimeSubstrateID != currentRuntime.RuntimeSubstrateID ||
		sourceRuntime.DeploymentDefinitionID != currentRuntime.DeploymentDefinitionID ||
		sourceRuntime.WorkspaceID != currentRuntime.WorkspaceID ||
		sourceRuntime.ProgramDeploymentID != currentRuntime.ProgramDeploymentID ||
		sourceRuntime.ReservedCpuMillis != currentRuntime.ReservedCpuMillis ||
		sourceRuntime.ReservedMemoryBytes != currentRuntime.ReservedMemoryBytes ||
		sourceRuntime.ReservedWorkloadDiskBytes != currentRuntime.ReservedWorkloadDiskBytes ||
		sourceRuntime.ReservedScratchBytes != currentRuntime.ReservedScratchBytes ||
		sourceRuntime.ReservedExecutionSlots != currentRuntime.ReservedExecutionSlots ||
		!bytes.Equal(sourceRuntime.NetworkPolicy, currentRuntime.NetworkPolicy) {
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
		wait.SuspendCheckpointID != locators.HandoffSuspendCheckpointID ||
		wait.HandoffResumeCheckpointID.Valid ||
		wait.ResumeAttachID != locators.HandoffResumeAttachID ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		wait.ResumeRequestVersion != wait.ResumeAckVersion ||
		wait.BaseWorkspaceVersionID != locators.HandoffBaseWorkspaceVersionID ||
		!wait.BaseWorkspaceContentDigest.Valid ||
		wait.ChildResultVersionID.Valid ||
		wait.ResumeWorkspaceVersionID.Valid ||
		wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
		wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
		wait.HandoffMountGeneration.Int64 != authority.workspaceMount.FencingGeneration ||
		wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
		wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64 ||
		wait.ChildWriterGeneration.Int64 >= wait.ResumeWriterGeneration.Int64 ||
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
		wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
		wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
		wait.HandoffMountGeneration.Int64 != authority.workspaceMount.FencingGeneration ||
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
