package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.RunStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("request body is required")
		}
		writeError(w, badRequest(fmt.Errorf("invalid worker run start request JSON: %w", err)))
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(w, badRequest(errors.New("invalid worker run start request JSON: trailing value")))
		return
	}
	leaseID, err := ids.Parse(request.Lease.ID)
	if err != nil || request.Lease.LeaseSequence <= 0 {
		writeError(w, badRequest(errors.New("lease.id must be a canonical UUIDv7 and lease.lease_sequence must be positive")))
		return
	}
	arm, err := parseRunStartArm(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	receipt, err := s.startRun(
		r.Context(), workerFromContext(r.Context()), pgvalue.UUID(leaseID), request.Lease, arm,
	)
	if err != nil {
		if errors.Is(err, errStaleRunLeaseClaim) {
			writeError(w, conflict(errors.New("run start acknowledgement is stale")))
			return
		}
		s.log.Error("start Run failed", "run_lease_id", request.Lease.ID, "error", err)
		writeError(w, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunStartResponse{Lease: receipt})
}

func (s *Server) startRun(
	ctx context.Context,
	worker workerActor,
	leaseID pgtype.UUID,
	expected workerapi.RunLeaseFence,
	requested runStartArm,
) (workerapi.RunLeaseFence, error) {
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetRunLeaseStartLocators(ctx, db.GetRunLeaseStartLocatorsParams{
			ID: leaseID, LeaseSequence: expected.LeaseSequence, WorkerGroupID: worker.WorkerGroupID,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
			WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		mode := deriveRunStartMode(locators)
		if mode != requested.mode {
			return errStaleRunLeaseClaim
		}
		authority, err := lockRunStartAuthority(ctx, work.q, worker, leaseID, expected.LeaseSequence, locators, mode)
		if err != nil {
			return err
		}
		if err := validateRunStartArm(requested, runStartValidationAuthority{
			run: authority.run, parentRun: authority.parentRun, runLease: authority.runLease,
			runtime: authority.runtime, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, runWait: authority.runWait,
		}); err != nil {
			return err
		}
		switch authority.runLease.State {
		case db.RunLeaseStateStarting:
			if authority.run.Status != db.RunStatusQueued || authority.attempt.EntrypointEnteredAt.Valid {
				return errStaleRunLeaseClaim
			}
			authority.runLease, err = work.q.MarkRunLeaseRunning(ctx, db.MarkRunLeaseRunningParams{
				ID: authority.runLease.ID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, LeaseSequence: authority.runLease.LeaseSequence,
				WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
				RuntimeInstanceID: authority.runtime.ID, RuntimeIdentityID: authority.runtime.RuntimeIdentityID,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
			authority.run, err = work.q.MarkRunRunning(ctx, db.MarkRunRunningParams{
				ID: authority.run.ID, OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
				ExpectedStateVersion: authority.run.StateVersion, AttemptNumber: authority.attempt.Number,
				RunLeaseID: authority.runLease.ID,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
			authority.workspace, err = work.q.TouchRunWorkspaceActivity(ctx, db.TouchRunWorkspaceActivityParams{
				ID: authority.workspace.ID, OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID:       authority.workspace.EnvironmentID,
				OwnershipGeneration: authority.workspaceLease.OwnershipGeneration,
				WriterGeneration:    authority.workspaceLease.WriterGeneration,
			})
			if err != nil {
				return staleRunLeaseClaim(err)
			}
		case db.RunLeaseStateRunning:
			if authority.run.Status != db.RunStatusRunning {
				return errStaleRunLeaseClaim
			}
		default:
			return errStaleRunLeaseClaim
		}
		return nil
	})
	if err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	return expected, nil
}

func lockRunStartAuthority(
	ctx context.Context, q db.Querier, worker workerActor, leaseID pgtype.UUID, leaseSequence int64,
	locators db.GetRunLeaseStartLocatorsRow, mode runLeaseClaimMode,
) (runLeaseClaimAuthority, error) {
	authority := runLeaseClaimAuthority{mode: mode}
	var err error
	if locators.SessionID.Valid {
		authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID: locators.SessionID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if authority.actor.CurrentRunID != locators.RunID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	}
	if locators.EnclosingWaitID.Valid {
		authority.parentRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
			ID: locators.ParentRunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
			EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
	}
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.run.CurrentAttemptNumber != locators.AttemptNumber || authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if mode == runLeaseClaimFresh && locators.SessionID.Valid &&
		(authority.run.EntrypointKind != "actor" || authority.run.SessionID != locators.SessionID) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.attempt.TerminalAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: worker.WorkerGroupID, RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if !authority.workerGroup.AllowsRun || authority.workerGroup.ClaimVersion != worker.GroupClaimVersion ||
		authority.workerGroup.ProtocolVersion != worker.ProtocolVersion {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.runLease, err = q.LockRunStartLease(ctx, db.LockRunStartLeaseParams{
		ID: leaseID, RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber, LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if authority.workerGroup.State != db.WorkerGroupStateActive &&
		!(authority.workerGroup.State == db.WorkerGroupStateDraining &&
			authority.runLease.State == db.RunLeaseStateRunning) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if locators.EnclosingWaitID.Valid && authority.runLease.State == db.RunLeaseStateStarting &&
		(authority.parentRun.Status != db.RunStatusWaiting ||
			authority.parentRun.CurrentRunLeaseID.Valid ||
			authority.run.ParentRunID != authority.parentRun.ID ||
			!authority.run.ParentOwnsLifecycle.Valid || !authority.run.ParentOwnsLifecycle.Bool ||
			authority.run.WorkspaceID != authority.parentRun.WorkspaceID ||
			authority.run.DeploymentID != authority.parentRun.DeploymentID) {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID, WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if err := validateRunLeaseWorkspaceAuthority(authority); err != nil {
		return runLeaseClaimAuthority{}, err
	}
	if locators.EnclosingWaitID.Valid {
		enclosingWait, err := q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
			ID: locators.EnclosingWaitID, EnvironmentID: locators.EnvironmentID,
			RunID: locators.ParentRunID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
		if mode == runLeaseClaimAttachChild {
			authority.runWait = enclosingWait
		} else {
			authority.enclosingWait = enclosingWait
		}
		if authority.runLease.State == db.RunLeaseStateStarting {
			if err := validateActiveEnclosingWait(
				enclosingWait, authority.run, authority.workspace.WriterGeneration, authority,
			); err != nil {
				return runLeaseClaimAuthority{}, err
			}
		}
	}
	if locators.RunWaitID.Valid {
		authority.runWait, err = q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
			ID: locators.RunWaitID, EnvironmentID: locators.EnvironmentID,
			RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
		}
	}
	if authority.runLease.State == db.RunLeaseStateStarting &&
		(mode == runLeaseClaimRestore || mode == runLeaseClaimAttachParent) {
		authority, err = lockRunStartCheckpointAuthority(ctx, q, mode, authority)
		if err != nil {
			return runLeaseClaimAuthority{}, err
		}
	}
	return authority, nil
}

func lockRunStartCheckpointAuthority(
	ctx context.Context,
	q db.Querier,
	mode runLeaseClaimMode,
	authority runLeaseClaimAuthority,
) (runLeaseClaimAuthority, error) {
	kind := db.RunCheckpointKindSuspend
	checkpointID := authority.runWait.SuspendCheckpointID
	sameWorkspaceParent := authority.runWait.ChildRunID.Valid &&
		authority.runWait.ChildParentOwned.Valid &&
		authority.runWait.ChildParentOwned.Bool
	if sameWorkspaceParent && authority.runWait.ConditionState == db.WaitStateCompleted {
		kind = db.RunCheckpointKindHandoffResume
		checkpointID = authority.runWait.HandoffResumeCheckpointID
	}
	var err error
	authority.checkpoint, err = q.LockReadyRunCheckpoint(ctx, db.LockReadyRunCheckpointParams{
		ID: checkpointID, Kind: kind, RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, RunWaitID: authority.runWait.ID,
		WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleRunLeaseClaim(err)
	}
	if mode == runLeaseClaimRestore && !sameWorkspaceParent {
		if err := validateCheckpointRestore(authority); err != nil {
			return runLeaseClaimAuthority{}, err
		}
	} else {
		if !sameWorkspaceParent {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		if err := validateSameWorkspaceParentResumeCheckpoint(authority); err != nil {
			return runLeaseClaimAuthority{}, err
		}
	}
	source, err := q.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: authority.checkpoint.SourceWorkspaceLeaseID,
		SourceRunLeaseID:       authority.checkpoint.SourceRunLeaseID,
		RunID:                  authority.run.ID,
		AttemptNumber:          authority.attempt.Number,
		WorkspaceID:            authority.workspace.ID,
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
	if mode == runLeaseClaimAttachParent {
		if authority.sourceRuntime.ID != authority.runtime.ID ||
			authority.sourceWorkspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
			authority.sourceWorkspaceLease.MountFencingGeneration != authority.runWait.HandoffMountGeneration.Int64 ||
			authority.sourceWorkspaceLease.WriterGeneration != authority.runWait.ParentWriterGeneration.Int64 {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
		return authority, nil
	}
	if authority.runtime.RestoreCheckpointID.Valid {
		if authority.runtime.RestoreCheckpointID != authority.checkpoint.ID {
			return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
		}
	} else if authority.sourceRuntime.ID != authority.runtime.ID ||
		authority.sourceWorkspaceLease.WorkspaceMountID != authority.workspaceMount.ID ||
		authority.sourceWorkspaceLease.MountFencingGeneration != authority.workspaceMount.FencingGeneration {
		return runLeaseClaimAuthority{}, errStaleRunLeaseClaim
	}
	return authority, nil
}

func validateRunStartArm(requested runStartArm, authority runStartValidationAuthority) error {
	if requested.mode == runLeaseClaimFresh {
		if authority.run.SessionID.Valid {
			if authority.run.EntrypointKind != "actor" {
				return errStaleRunLeaseClaim
			}
		} else if authority.run.EntrypointKind != "task" {
			return errStaleRunLeaseClaim
		}
		return nil
	}
	wait := authority.runWait
	if wait.ID != requested.runWaitID || wait.ResumeAttachID != requested.resumeAttachID {
		return errStaleRunLeaseClaim
	}
	if wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion {
		return errStaleRunLeaseClaim
	}
	checkpointID := wait.SuspendCheckpointID
	sameWorkspaceParent := wait.ChildRunID.Valid &&
		wait.ChildParentOwned.Valid &&
		wait.ChildParentOwned.Bool
	if sameWorkspaceParent && wait.ConditionState == db.WaitStateCompleted {
		checkpointID = wait.HandoffResumeCheckpointID
	}
	if checkpointID != requested.checkpointID {
		return errStaleRunLeaseClaim
	}
	switch requested.mode {
	case runLeaseClaimRestore:
		if wait.ConditionState == db.WaitStatePending ||
			wait.CurrentRunLeaseID != authority.runLease.ID ||
			(wait.SuspensionState != db.RunWaitStateResuming &&
				!(authority.runLease.State == db.RunLeaseStateRunning && wait.SuspensionState == db.RunWaitStateReleased)) ||
			wait.ResumeRequestVersion != requested.resumeRequestVersion ||
			(wait.SuspensionState == db.RunWaitStateResuming && wait.ResumeAckVersion >= wait.ResumeRequestVersion) ||
			(wait.SuspensionState == db.RunWaitStateReleased && wait.ResumeAckVersion != wait.ResumeRequestVersion) {
			return errStaleRunLeaseClaim
		}
		if sameWorkspaceParent {
			if authority.runtime.RestoreCheckpointID != requested.checkpointID ||
				(wait.ConditionState != db.WaitStateCompleted &&
					wait.ConditionState != db.WaitStateFailed &&
					wait.ConditionState != db.WaitStateCancelled) {
				return errStaleRunLeaseClaim
			}
		} else if wait.HandoffResumeCheckpointID.Valid {
			return errStaleRunLeaseClaim
		}
	case runLeaseClaimAttachParent:
		if (wait.ConditionState != db.WaitStateCompleted &&
			wait.ConditionState != db.WaitStateFailed &&
			wait.ConditionState != db.WaitStateCancelled) ||
			wait.Kind != db.WaitKindChild || !wait.ChildRunID.Valid ||
			!wait.ChildParentOwned.Valid || !wait.ChildParentOwned.Bool ||
			wait.CurrentRunLeaseID != authority.runLease.ID ||
			(wait.SuspensionState != db.RunWaitStateResuming &&
				!(authority.runLease.State == db.RunLeaseStateRunning && wait.SuspensionState == db.RunWaitStateReleased)) ||
			wait.ResumeRequestVersion != requested.resumeRequestVersion ||
			(wait.SuspensionState == db.RunWaitStateResuming && wait.ResumeAckVersion >= wait.ResumeRequestVersion) ||
			(wait.SuspensionState == db.RunWaitStateReleased && wait.ResumeAckVersion != wait.ResumeRequestVersion) ||
			wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
			wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
			!wait.HandoffMountGeneration.Valid ||
			!wait.OwnershipGeneration.Valid ||
			wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
			!wait.ResumeWriterGeneration.Valid ||
			wait.ResumeWriterGeneration.Int64 != authority.workspace.WriterGeneration {
			return errStaleRunLeaseClaim
		}
	case runLeaseClaimAttachChild:
		if wait.Kind != db.WaitKindChild || !wait.ChildRunID.Valid ||
			wait.ChildRunID != authority.run.ID ||
			!wait.ChildParentOwned.Valid || !wait.ChildParentOwned.Bool ||
			authority.run.ParentRunID != authority.parentRun.ID {
			return errStaleRunLeaseClaim
		}
		if authority.runLease.State == db.RunLeaseStateRunning {
			return nil
		}
		if wait.ConditionState != db.WaitStatePending ||
			wait.SuspensionState != db.RunWaitStateParked || wait.CurrentRunLeaseID.Valid ||
			authority.parentRun.Status != db.RunStatusWaiting ||
			wait.HandoffRuntimeInstanceID != authority.runtime.ID ||
			wait.HandoffWorkspaceMountID != authority.workspaceMount.ID ||
			!wait.HandoffMountGeneration.Valid ||
			!wait.OwnershipGeneration.Valid ||
			wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
			!wait.ChildWriterGeneration.Valid ||
			wait.ChildWriterGeneration.Int64 != authority.workspace.WriterGeneration ||
			wait.ResumeRequestVersion != wait.ResumeAckVersion {
			return errStaleRunLeaseClaim
		}
	default:
		return errStaleRunLeaseClaim
	}
	return nil
}
