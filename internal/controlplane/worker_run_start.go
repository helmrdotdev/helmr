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
			if point, ok := staleAuthorityPointOf(err); ok {
				s.log.Warn(
					"run start acknowledgement is stale",
					"failure_point", point,
					"run_lease_id", request.Lease.ID,
					"lease_sequence", request.Lease.LeaseSequence,
					"worker_group_id", workerFromContext(r.Context()).WorkerGroupID,
					"worker_instance_id", workerFromContext(r.Context()).WorkerInstanceID,
					"worker_epoch", workerFromContext(r.Context()).WorkerEpoch,
				)
			}
			writeError(w, conflict(err))
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
			ID: leaseID, LeaseSequence: expected.LeaseSequence, WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerEpoch: worker.WorkerEpoch,
		})
		if err != nil {
			return staleAuthority(staleAuthorityRunStart, runStartFailureLocators, staleRunLeaseClaim(err))
		}
		mode := deriveRunStartMode(locators)
		if mode != requested.mode {
			return staleAuthority(staleAuthorityRunStart, runStartFailureMode, errStaleRunLeaseClaim)
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
			return staleAuthority(staleAuthorityRunStart, runStartFailureArm, err)
		}
		switch authority.runLease.State {
		case db.RunLeaseStateStarting:
			if err := validateRunStartLifecycle(mode, authority.run, authority.attempt); err != nil {
				return staleAuthority(staleAuthorityRunStart, runStartFailureLeaseState, errStaleRunLeaseClaim)
			}
			authority.runLease, err = work.q.MarkRunLeaseRunning(ctx, db.MarkRunLeaseRunningParams{
				ID: authority.runLease.ID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
				AttemptNumber: authority.attempt.Number, LeaseSequence: authority.runLease.LeaseSequence,
				WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
				WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: authority.runtime.ID, RuntimeIdentityID: authority.runtime.RuntimeIdentityID,
			})
			if err != nil {
				return staleAuthority(staleAuthorityRunStart, runStartFailureMarkLeaseRunning, staleRunLeaseClaim(err))
			}
			authority.run, err = work.q.MarkRunRunning(ctx, db.MarkRunRunningParams{
				ID: authority.run.ID, OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID: authority.run.EnvironmentID, WorkspaceID: authority.workspace.ID,
				ExpectedStateVersion: authority.run.StateVersion, AttemptNumber: authority.attempt.Number,
				RunLeaseID: authority.runLease.ID,
			})
			if err != nil {
				return staleAuthority(staleAuthorityRunStart, runStartFailureMarkRunRunning, staleRunLeaseClaim(err))
			}
			authority.workspace, err = work.q.TouchRunWorkspaceActivity(ctx, db.TouchRunWorkspaceActivityParams{
				ID: authority.workspace.ID, OrgID: authority.run.OrgID, ProjectID: authority.run.ProjectID,
				EnvironmentID:       authority.workspace.EnvironmentID,
				OwnershipGeneration: authority.workspaceLease.OwnershipGeneration,
				WriterGeneration:    authority.workspaceLease.WriterGeneration,
			})
			if err != nil {
				return staleAuthority(staleAuthorityRunStart, runStartFailureTouchWorkspace, staleRunLeaseClaim(err))
			}
		case db.RunLeaseStateRunning:
			if authority.run.Status != db.RunStatusRunning {
				return staleAuthority(staleAuthorityRunStart, runStartFailureLeaseState, errStaleRunLeaseClaim)
			}
		default:
			return staleAuthority(staleAuthorityRunStart, runStartFailureLeaseState, errStaleRunLeaseClaim)
		}
		return nil
	})
	if err != nil {
		return workerapi.RunLeaseFence{}, err
	}
	return expected, nil
}

func validateRunStartLifecycle(mode runLeaseClaimMode, run db.Run, attempt db.RunAttempt) error {
	if run.Status != db.RunStatusQueued {
		return errStaleRunLeaseClaim
	}
	switch mode {
	case runLeaseClaimFresh:
		if attempt.EntrypointEnteredAt.Valid {
			return errStaleRunLeaseClaim
		}
	case runLeaseClaimRestore:
		if !attempt.EntrypointEnteredAt.Valid {
			return errStaleRunLeaseClaim
		}
	default:
		return errStaleRunLeaseClaim
	}
	return nil
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
			return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRun, staleRunLeaseClaim(err))
		}
		if authority.actor.CurrentRunID != locators.RunID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") {
			return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRun, errStaleRunLeaseClaim)
		}
	}
	if locators.EnclosingWaitID.Valid {
		authority.parentRun, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
			ID: locators.ParentRunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
			EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureParentWait, staleRunLeaseClaim(err))
		}
	}
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRun, staleRunLeaseClaim(err))
	}
	if authority.run.CurrentAttemptNumber != locators.AttemptNumber || authority.run.CurrentRunLeaseID != leaseID {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRun, errStaleRunLeaseClaim)
	}
	if mode == runLeaseClaimFresh && locators.SessionID.Valid &&
		(authority.run.EntrypointKind != "actor" || authority.run.SessionID != locators.SessionID) {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRun, errStaleRunLeaseClaim)
	}
	authority.workspace, err = q.LockRunLeaseClaimWorkspace(ctx, db.LockRunLeaseClaimWorkspaceParams{
		ID: locators.WorkspaceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkspace, staleRunLeaseClaim(err))
	}
	if authority.workspace.State != db.WorkspaceStateActive ||
		authority.workspace.DesiredState != db.WorkspaceDesiredStateActive {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkspace, errStaleRunLeaseClaim)
	}
	authority.attempt, err = q.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: locators.RunID, Number: locators.AttemptNumber, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureAttempt, staleRunLeaseClaim(err))
	}
	if authority.attempt.TerminalAt.Valid ||
		authority.attempt.EntrypointKind != authority.run.EntrypointKind ||
		authority.attempt.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureAttempt, errStaleRunLeaseClaim)
	}
	authority.workerGroup, err = q.LockRunLeaseClaimWorkerGroup(ctx, db.LockRunLeaseClaimWorkerGroupParams{
		ID: pgvalue.UUID(worker.WorkerGroupID), RegionID: locators.RegionID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkerGroup, staleRunLeaseClaim(err))
	}
	if authority.workerGroup.ClaimVersion != worker.GroupClaimVersion {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkerGroup, errStaleRunLeaseClaim)
	}
	authority.worker, err = q.LockRunLeaseClaimWorker(ctx, db.LockRunLeaseClaimWorkerParams{
		ID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorker, staleRunLeaseClaim(err))
	}
	if err := validateClaimWorker(worker, authority.worker); err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorker, err)
	}
	authority.runtime, err = q.LockRunLeaseClaimRuntime(ctx, db.LockRunLeaseClaimRuntimeParams{
		ID: locators.RuntimeInstanceID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRuntime, staleRunLeaseClaim(err))
	}
	authority.runLease, err = q.LockRunStartLease(ctx, db.LockRunStartLeaseParams{
		ID: leaseID, RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		AttemptNumber: locators.AttemptNumber, LeaseSequence: leaseSequence,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRunLease, staleRunLeaseClaim(err))
	}
	if authority.workerGroup.State != db.WorkerGroupStateActive &&
		authority.workerGroup.State != db.WorkerGroupStateDraining {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkerGroup, errStaleRunLeaseClaim)
	}
	if err := validateClaimPhysicalAuthority(worker, authority); err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailurePhysicalAuthority, err)
	}
	if locators.EnclosingWaitID.Valid && authority.runLease.State == db.RunLeaseStateStarting &&
		(authority.parentRun.Status != db.RunStatusWaiting ||
			authority.parentRun.CurrentRunLeaseID.Valid ||
			authority.run.ParentRunID != authority.parentRun.ID ||
			!authority.run.ParentOwnsLifecycle.Valid || !authority.run.ParentOwnsLifecycle.Bool ||
			authority.run.WorkspaceID != authority.parentRun.WorkspaceID ||
			authority.run.DeploymentID != authority.parentRun.DeploymentID) {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureParentWait, errStaleRunLeaseClaim)
	}
	authority.workspaceMount, err = q.LockRunLeaseClaimMount(ctx, db.LockRunLeaseClaimMountParams{
		ID: locators.WorkspaceMountID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkspaceMount, staleRunLeaseClaim(err))
	}
	authority.workspaceLease, err = q.LockRunLeaseClaimWorkspaceLease(ctx, db.LockRunLeaseClaimWorkspaceLeaseParams{
		ID: locators.WorkspaceLeaseID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, RegionID: locators.RegionID,
		WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch, RuntimeInstanceID: locators.RuntimeInstanceID,
		WorkspaceID: locators.WorkspaceID, WorkspaceMountID: locators.WorkspaceMountID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkspaceLease, staleRunLeaseClaim(err))
	}
	if err := validateRunLeaseWorkspaceAuthority(authority); err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWorkspaceAuthority, err)
	}
	if locators.EnclosingWaitID.Valid {
		enclosingWait, err := q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
			ID: locators.EnclosingWaitID, EnvironmentID: locators.EnvironmentID,
			RunID: locators.ParentRunID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureEnclosingWait, staleRunLeaseClaim(err))
		}
		authority.enclosingWait = enclosingWait
		if authority.runLease.State == db.RunLeaseStateStarting {
			if err := validateActiveEnclosingWait(
				enclosingWait, authority.run, authority.workspace.WriterGeneration, authority,
			); err != nil {
				return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureEnclosingWait, err)
			}
		}
	}
	if locators.RunWaitID.Valid {
		authority.runWait, err = q.LockRunStartWait(ctx, db.LockRunStartWaitParams{
			ID: locators.RunWaitID, EnvironmentID: locators.EnvironmentID,
			RunID: locators.RunID, WorkspaceID: locators.WorkspaceID,
		})
		if err != nil {
			return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureWait, staleRunLeaseClaim(err))
		}
	}
	if authority.runLease.State == db.RunLeaseStateStarting && mode == runLeaseClaimRestore {
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
	if mode != runLeaseClaimRestore {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureCheckpointValidation, errStaleRunLeaseClaim)
	}
	var err error
	authority.checkpoint, err = q.LockReadyRunCheckpoint(ctx, db.LockReadyRunCheckpointParams{
		ID: authority.runWait.SuspendCheckpointID, RunID: authority.run.ID,
		AttemptNumber: authority.attempt.Number, RunWaitID: authority.runWait.ID,
		WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureCheckpoint, staleRunLeaseClaim(err))
	}
	if err := validateCheckpointRestore(authority); err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureCheckpointValidation, err)
	}
	source, err := q.GetRunCheckpointSource(ctx, db.GetRunCheckpointSourceParams{
		SourceWorkspaceLeaseID: authority.checkpoint.SourceWorkspaceLeaseID,
		SourceRunLeaseID:       authority.checkpoint.SourceRunLeaseID,
		RunID:                  authority.run.ID,
		AttemptNumber:          authority.attempt.Number,
		WorkspaceID:            authority.workspace.ID,
	})
	if err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureCheckpointSource, staleRunLeaseClaim(err))
	}
	authority.sourceRunLease = source.RunLease
	authority.sourceWorkspaceLease = source.WorkspaceLease
	authority.sourceRuntime = source.RuntimeInstance
	if err := validateCheckpointSource(authority); err != nil {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureSourceValidation, err)
	}
	if authority.runtime.RestoreCheckpointID != authority.checkpoint.ID {
		return runLeaseClaimAuthority{}, staleAuthority(staleAuthorityRunStart, runStartFailureRestoreBinding, errStaleRunLeaseClaim)
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
	if requested.mode != runLeaseClaimRestore {
		return errStaleRunLeaseClaim
	}
	wait := authority.runWait
	if wait.ID != requested.runWaitID || wait.ResumeAttachID != requested.resumeAttachID {
		return errStaleRunLeaseClaim
	}
	if wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion {
		return errStaleRunLeaseClaim
	}
	if wait.SuspendCheckpointID != requested.checkpointID {
		return errStaleRunLeaseClaim
	}
	if wait.ConditionState == db.WaitStatePending ||
		wait.CurrentRunLeaseID != authority.runLease.ID ||
		(wait.SuspensionState != db.RunWaitStateResuming &&
			!(authority.runLease.State == db.RunLeaseStateRunning && wait.SuspensionState == db.RunWaitStateReleased)) ||
		wait.ResumeRequestVersion != requested.resumeRequestVersion ||
		(wait.SuspensionState == db.RunWaitStateResuming && wait.ResumeAckVersion >= wait.ResumeRequestVersion) ||
		(wait.SuspensionState == db.RunWaitStateReleased && wait.ResumeAckVersion != wait.ResumeRequestVersion) ||
		authority.runtime.RestoreCheckpointID != requested.checkpointID {
		return errStaleRunLeaseClaim
	}
	return nil
}
