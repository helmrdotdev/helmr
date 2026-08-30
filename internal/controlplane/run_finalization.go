package controlplane

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

var errStaleRunFinalization = errors.New("run finalization authority is stale")

type parsedRunFinalization struct {
	lease       parsedRunLeaseFence
	runID       uuid.UUID
	attempt     int32
	operationID uuid.UUID
	kind        workerapi.RunFinalizationKind
	fingerprint string
}

func parseRunFinalization(request workerapi.BeginRunFinalizationRequest) (parsedRunFinalization, error) {
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	if request.Kind != workerapi.RunFinalizationCapture && request.Kind != workerapi.RunFinalizationReset {
		return parsedRunFinalization{}, errors.New("kind must be capture or reset")
	}
	quiescedRunID, err := parseCanonicalUUID("program_quiesced.run_id", request.ProgramQuiesced.RunID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	quiescedLeaseID, err := parseCanonicalUUID("program_quiesced.run_lease_id", request.ProgramQuiesced.RunLeaseID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	if quiescedLeaseID != lease.leaseID ||
		request.ProgramQuiesced.AttemptNumber <= 0 {
		return parsedRunFinalization{}, errors.New("program_quiesced does not match the run lease")
	}
	normalized := request
	normalized.OperationID = operationID.String()
	normalized.ProgramQuiesced.RunID = quiescedRunID.String()
	normalized.ProgramQuiesced.RunLeaseID = quiescedLeaseID.String()
	fingerprint, err := terminalRequestFingerprint("run.finalization.begin.v0", normalized)
	if err != nil {
		return parsedRunFinalization{}, fmt.Errorf("fingerprint run finalization: %w", err)
	}
	return parsedRunFinalization{
		lease: lease, runID: quiescedRunID, attempt: request.ProgramQuiesced.AttemptNumber,
		operationID: operationID, kind: request.Kind, fingerprint: fingerprint,
	}, nil
}

func (s *Server) beginRunFinalization(
	ctx context.Context,
	worker workerActor,
	request workerapi.BeginRunFinalizationRequest,
	parsed parsedRunFinalization,
) (workerapi.BeginRunFinalizationResponse, error) {
	var response workerapi.BeginRunFinalizationResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleRunFinalization(err)
		}
		if locators.RunID != pgvalue.UUID(parsed.runID) ||
			locators.AttemptNumber != parsed.attempt {
			return errStaleRunFinalization
		}
		if _, err := secret.LockAttemptDelivery(
			ctx,
			work.q,
			locators.RunID,
			locators.AttemptNumber,
			locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock run finalization secret authority: %w", err)
		}
		authority, err := lockLiveRunFinalizationAuthority(
			ctx,
			work.q,
			worker,
			pgvalue.UUID(parsed.lease.leaseID),
			request.Lease.LeaseSequence,
			locators,
		)
		if err != nil {
			return staleRunFinalization(err)
		}
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return err
		}
		if err := lockSameWorkspaceChildFinalization(ctx, work.q, &authority); err != nil {
			return err
		}
		if !authority.attempt.EntrypointEnteredAt.Valid {
			return errStaleRunFinalization
		}
		current, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			runLease:  authority.runLease,
			workspace: authority.workspace, workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil {
			return err
		}
		if authority.runLease.State == db.RunLeaseStateFinalizing {
			if authority.run.ActiveStartedAt.Valid ||
				!authority.runLease.FinalizationOperationID.Valid ||
				authority.runLease.FinalizationOperationID != pgvalue.UUID(parsed.operationID) ||
				authority.runLease.FinalizationKind.String != string(parsed.kind) ||
				authority.runLease.FinalizationRequestFingerprint.String != parsed.fingerprint ||
				!authority.runLease.FinalizationStartedAt.Valid {
				return errStaleRunFinalization
			}
			response = workerapi.BeginRunFinalizationResponse{
				Lease: request.Lease, BaseWorkspaceVersionID: current.BaseWorkspaceVersionID,
				ExpiresAt: current.ExpiresAt.UTC(), OperationID: parsed.operationID.String(), Kind: parsed.kind,
				StartedAt: authority.runLease.FinalizationStartedAt.Time.UTC(),
			}
			return nil
		}
		if authority.runLease.State != db.RunLeaseStateRunning ||
			!authority.run.ActiveStartedAt.Valid ||
			authority.runLease.FinalizationOperationID.Valid ||
			authority.runLease.FinalizationKind.Valid ||
			authority.runLease.FinalizationStartedAt.Valid ||
			authority.runLease.FinalizationRequestFingerprint.Valid {
			return errStaleRunFinalization
		}
		clear, err := work.q.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
			RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
		})
		if err != nil {
			return err
		}
		if !clear.Valid || !clear.Bool {
			return errStaleRunFinalization
		}
		now, err := work.q.GetRunFinalizationTime(ctx)
		if err != nil || !now.Valid {
			if err == nil {
				err = errors.New("database run finalization time is unavailable")
			}
			return err
		}
		if !now.Time.Before(authority.runLease.ExpiresAt.Time) ||
			!now.Time.Before(authority.workspaceLease.ExpiresAt.Time) ||
			authority.run.MaxActiveDurationMs <= 0 ||
			authority.run.ActiveElapsedMs < 0 ||
			authority.run.ActiveElapsedMs >= authority.run.MaxActiveDurationMs {
			return errStaleRunFinalization
		}
		remaining := authority.run.MaxActiveDurationMs - authority.run.ActiveElapsedMs
		hardDeadline := authority.run.ActiveStartedAt.Time.Add(time.Duration(remaining) * time.Millisecond)
		if !now.Time.Before(hardDeadline) {
			return errStaleRunFinalization
		}
		expiresAt := now.Time.Add(run.FinalizationTTL)
		minimum := authority.runLease.ExpiresAt.Time.Add(time.Microsecond)
		if expiresAt.Before(minimum) {
			expiresAt = minimum
		}
		startedAt := pgvalue.Timestamptz(now.Time)
		authority.run, err = work.q.CloseRunActiveIntervalForFinalization(ctx, db.CloseRunActiveIntervalForFinalizationParams{
			FinalizationStartedAt: startedAt, ID: authority.run.ID, OrgID: authority.run.OrgID,
			ProjectID: authority.run.ProjectID, EnvironmentID: authority.run.EnvironmentID,
			WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
			RunLeaseID: authority.runLease.ID, ExpectedStateVersion: authority.run.StateVersion,
		})
		if err != nil {
			return staleRunFinalization(err)
		}
		previousExpiry := authority.runLease.ExpiresAt
		authority.runLease, err = work.q.BeginRunLeaseFinalization(ctx, db.BeginRunLeaseFinalizationParams{
			ExpiresAt: pgvalue.Timestamptz(expiresAt), FinalizationOperationID: pgvalue.UUID(parsed.operationID),
			FinalizationKind: pgvalue.Text(string(parsed.kind)), FinalizationStartedAt: startedAt,
			FinalizationRequestFingerprint: pgvalue.Text(parsed.fingerprint), ID: authority.runLease.ID,
			RunID: authority.run.ID, WorkspaceID: authority.workspace.ID, AttemptNumber: authority.attempt.Number,
			LeaseSequence: authority.runLease.LeaseSequence, PreviousExpiresAt: previousExpiry,
		})
		if err != nil {
			return staleRunFinalization(err)
		}
		authority.workspaceLease, err = work.q.BeginRunWorkspaceLeaseFinalization(ctx, db.BeginRunWorkspaceLeaseFinalizationParams{
			ExpiresAt: pgvalue.Timestamptz(expiresAt), FinalizationStartedAt: startedAt,
			ID: authority.workspaceLease.ID, WorkspaceID: authority.workspace.ID,
			RuntimeInstanceID: authority.runtime.ID, WorkspaceMountID: authority.workspaceMount.ID,
			OwnerRunLeaseID: authority.runLease.ID, OwnershipGeneration: authority.workspaceLease.OwnershipGeneration,
			WriterGeneration:       authority.workspaceLease.WriterGeneration,
			MountFencingGeneration: authority.workspaceLease.MountFencingGeneration,
			PreviousExpiresAt:      previousExpiry,
		})
		if err != nil {
			return staleRunFinalization(err)
		}
		frozen, err := projectRunLeaseAssignment(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			runLease:  authority.runLease,
			workspace: authority.workspace, workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil {
			return err
		}
		response = workerapi.BeginRunFinalizationResponse{
			Lease: request.Lease, BaseWorkspaceVersionID: frozen.BaseWorkspaceVersionID,
			ExpiresAt: frozen.ExpiresAt.UTC(), OperationID: parsed.operationID.String(), Kind: parsed.kind,
			StartedAt: now.Time.UTC(),
		}
		return nil
	})
	return response, err
}

type runFinalizationOwner struct {
	actor  db.Session
	parent db.Run
}

func lockRunFinalizationOwner(
	ctx context.Context,
	q db.Querier,
	locators db.GetLiveRunLeaseLocatorsRow,
) (runFinalizationOwner, error) {
	var owner runFinalizationOwner
	var err error
	switch {
	case locators.SessionID.Valid && !locators.ParentRunID.Valid && !locators.ParentOwnsLifecycle.Valid:
		owner.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID: locators.SessionID, WorkspaceID: locators.WorkspaceID,
		})
	case !locators.SessionID.Valid && locators.ParentRunID.Valid && locators.ParentOwnsLifecycle.Valid && locators.ParentOwnsLifecycle.Bool:
		owner.parent, err = q.LockRunFinalizationParentRun(ctx, db.LockRunFinalizationParentRunParams{
			ID: locators.ParentRunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
			EnvironmentID: locators.EnvironmentID,
		})
	case !locators.SessionID.Valid &&
		((!locators.ParentRunID.Valid && !locators.ParentOwnsLifecycle.Valid) ||
			(locators.ParentRunID.Valid && locators.ParentOwnsLifecycle.Valid && !locators.ParentOwnsLifecycle.Bool)):
	default:
		return owner, errStaleRunFinalization
	}
	if err != nil {
		return runFinalizationOwner{}, staleRunFinalization(err)
	}
	return owner, nil
}

func lockLiveRunFinalizationAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	leaseID pgtype.UUID,
	leaseSequence int64,
	locators db.GetLiveRunLeaseLocatorsRow,
) (runLeaseClaimAuthority, error) {
	var authority runLeaseClaimAuthority
	var lineage []db.ListSameWorkspaceAncestorRunsRow
	if !locators.ParentRunID.Valid ||
		!locators.ParentOwnsLifecycle.Valid ||
		!locators.ParentOwnsLifecycle.Bool {
		owner, err := lockRunFinalizationOwner(ctx, q, locators)
		if err != nil {
			return authority, err
		}
		authority.actor = owner.actor
		authority.parentRun = owner.parent
	} else {
		var err error
		lineage, err = q.ListSameWorkspaceAncestorRuns(
			ctx,
			db.ListSameWorkspaceAncestorRunsParams{
				EnvironmentID: locators.EnvironmentID,
				WorkspaceID:   locators.WorkspaceID,
				ChildRunID:    locators.RunID,
			},
		)
		if err != nil {
			return authority, staleRunFinalization(err)
		}
		if len(lineage) == 0 {
			owner, err := lockRunFinalizationOwner(ctx, q, locators)
			if err != nil {
				return authority, err
			}
			authority.actor = owner.actor
			authority.parentRun = owner.parent
		} else if lineage[0].Run.EntrypointKind == "actor" {
			root := lineage[0].Run
			if !root.SessionID.Valid || root.ParentRunID.Valid || root.ParentOwnsLifecycle.Valid {
				return authority, errStaleRunFinalization
			}
			authority.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
				ID: root.SessionID, WorkspaceID: locators.WorkspaceID,
			})
			if err != nil {
				return authority, staleRunFinalization(err)
			}
			if authority.actor.ID != root.SessionID ||
				authority.actor.CurrentRunID != root.ID ||
				(authority.actor.State != "open" &&
					authority.actor.State != "closing") {
				return authority, errStaleRunFinalization
			}
		}
	}

	for index, discovered := range lineage {
		if discovered.Depth != int32(len(lineage)-index-1) ||
			(index > 0 && discovered.Run.ParentRunID != lineage[index-1].Run.ID) {
			return authority, errStaleRunFinalization
		}
		locked, err := q.LockRunFinalizationParentRun(
			ctx,
			db.LockRunFinalizationParentRunParams{
				ID: discovered.Run.ID, OrgID: discovered.Run.OrgID,
				ProjectID:     discovered.Run.ProjectID,
				EnvironmentID: discovered.Run.EnvironmentID,
			},
		)
		if err != nil {
			return authority, staleRunFinalization(err)
		}
		if locked.ID != discovered.Run.ID ||
			locked.WorkspaceID != locators.WorkspaceID ||
			locked.Status != db.RunStatusWaiting ||
			locked.CurrentRunLeaseID.Valid {
			return authority, errStaleRunFinalization
		}
		authority.parentRun = locked
	}
	if len(lineage) > 0 && authority.parentRun.ID != locators.ParentRunID {
		return authority, errStaleRunFinalization
	}

	var err error
	authority.run, err = q.LockRunLeaseClaimRun(ctx, db.LockRunLeaseClaimRunParams{
		ID: locators.RunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
		EnvironmentID: locators.EnvironmentID, WorkspaceID: locators.WorkspaceID,
	})
	if err != nil {
		return authority, staleRunFinalization(err)
	}
	if err := validateLockedRunLeaseRun(
		authority.run, leaseID, locators, db.RunStatusRunning,
	); err != nil {
		return authority, staleRunFinalization(err)
	}
	if err := lockRunLeaseWorkspace(ctx, q, &authority, locators); err != nil {
		return authority, staleRunFinalization(err)
	}

	for _, discovered := range lineage {
		attempt, err := q.LockRunLeaseClaimAttempt(
			ctx,
			db.LockRunLeaseClaimAttemptParams{
				RunID: discovered.Run.ID, Number: discovered.Run.CurrentAttemptNumber,
				WorkspaceID: locators.WorkspaceID,
			},
		)
		if err != nil {
			return authority, staleRunFinalization(err)
		}
		if attempt.RunID != discovered.Run.ID ||
			attempt.Number != discovered.Run.CurrentAttemptNumber ||
			attempt.WorkspaceID != locators.WorkspaceID ||
			attempt.TerminalAt.Valid {
			return authority, errStaleRunFinalization
		}
	}
	if err := lockRunLeaseAttempt(ctx, q, &authority, locators); err != nil {
		return authority, staleRunFinalization(err)
	}
	if err := lockRunLeasePhysicalAuthority(
		ctx, q, worker, leaseID, leaseSequence, locators, &authority,
	); err != nil {
		return authority, staleRunFinalization(err)
	}
	return authority, nil
}

func validateRunFinalizationOwner(
	authority runLeaseClaimAuthority,
	locators db.GetLiveRunLeaseLocatorsRow,
) error {
	switch authority.run.EntrypointKind {
	case "actor":
		if !locators.SessionID.Valid || authority.run.SessionID != locators.SessionID ||
			authority.actor.ID != locators.SessionID || authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") ||
			authority.run.ParentRunID.Valid || authority.run.ParentOwnsLifecycle.Valid {
			return errStaleRunFinalization
		}
	case "task":
		if authority.run.SessionID.Valid || authority.run.ParentRunID != locators.ParentRunID ||
			authority.run.ParentOwnsLifecycle != locators.ParentOwnsLifecycle {
			return errStaleRunFinalization
		}
		if locators.ParentRunID.Valid && locators.ParentOwnsLifecycle.Bool {
			if authority.parentRun.ID != locators.ParentRunID ||
				(authority.parentRun.Status != db.RunStatusQueued &&
					authority.parentRun.Status != db.RunStatusRunning &&
					authority.parentRun.Status != db.RunStatusWaiting &&
					authority.parentRun.Status != db.RunStatusRetryDelayed &&
					authority.parentRun.Status != db.RunStatusCancelRequested) {
				return errStaleRunFinalization
			}
		}
	default:
		return errStaleRunFinalization
	}
	return nil
}

func lockSameWorkspaceChildFinalization(
	ctx context.Context,
	store db.Querier,
	authority *runLeaseClaimAuthority,
) error {
	if !authority.run.ParentRunID.Valid ||
		!authority.run.ParentOwnsLifecycle.Valid ||
		!authority.run.ParentOwnsLifecycle.Bool ||
		authority.parentRun.WorkspaceID != authority.run.WorkspaceID {
		return nil
	}
	var err error
	if authority.parentRun.ParentRunID.Valid &&
		authority.parentRun.ParentOwnsLifecycle.Valid &&
		authority.parentRun.ParentOwnsLifecycle.Bool {
		authority.sameWorkspaceAncestors, err = store.LockSameWorkspaceAncestors(
			ctx,
			db.LockSameWorkspaceAncestorsParams{
				EnvironmentID: authority.run.EnvironmentID,
				ChildRunID:    authority.parentRun.ID,
				WorkspaceID:   authority.workspace.ID,
			},
		)
		if err != nil {
			return staleRunFinalization(err)
		}
		if len(authority.sameWorkspaceAncestors) == 0 {
			return errStaleRunFinalization
		}
		authority.parentEnclosingWait =
			authority.sameWorkspaceAncestors[len(authority.sameWorkspaceAncestors)-1].RunWait
	}
	wait, err := store.LockParentOwnedChildWait(ctx, db.LockParentOwnedChildWaitParams{
		EnvironmentID: authority.run.EnvironmentID,
		ParentRunID:   authority.parentRun.ID,
		ChildRunID:    authority.run.ID,
	})
	if err != nil {
		return staleRunFinalization(err)
	}
	authority.parentAttempt, err = store.LockRunLeaseClaimAttempt(ctx, db.LockRunLeaseClaimAttemptParams{
		RunID: authority.parentRun.ID, Number: wait.AttemptNumber, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return staleRunFinalization(err)
	}
	authority.runWait = wait
	authority.enclosingWait = wait
	if err := validateSameWorkspaceChildFinalization(*authority); err != nil {
		return err
	}
	return nil
}

func validateSameWorkspaceChildFinalization(authority runLeaseClaimAuthority) error {
	wait := authority.runWait
	if authority.parentRun.Status != db.RunStatusWaiting ||
		authority.parentRun.CurrentAttemptNumber != authority.parentAttempt.Number ||
		authority.parentRun.CurrentRunLeaseID.Valid ||
		authority.parentAttempt.TerminalAt.Valid ||
		!authority.parentAttempt.EntrypointEnteredAt.Valid ||
		wait.Kind != db.WaitKindChild ||
		wait.ConditionState != db.WaitStatePending ||
		wait.SuspensionState != db.RunWaitStateParked ||
		wait.RunID != authority.parentRun.ID ||
		wait.WorkspaceID != authority.workspace.ID ||
		wait.ChildRunID != authority.run.ID ||
		!wait.ChildParentOwned.Valid ||
		!wait.ChildParentOwned.Bool ||
		!wait.ChildTargetDeclaredID.Valid ||
		wait.ChildTargetDeclaredID.String != authority.run.EntrypointDeclaredID ||
		wait.CurrentRunLeaseID.Valid ||
		!wait.PriorRunLeaseID.Valid ||
		!wait.SuspendCheckpointID.Valid ||
		!wait.ResumeAttachID.Valid ||
		wait.CheckpointRequestVersion <= 0 ||
		wait.CheckpointRequestVersion != wait.CheckpointAckVersion ||
		wait.ResumeRequestVersion != wait.ResumeAckVersion ||
		wait.BaseWorkspaceVersionID != authority.run.BaseWorkspaceVersionID ||
		!wait.BaseWorkspaceContentDigest.Valid ||
		wait.ResumeWorkspaceVersionID.Valid ||
		!wait.OwnershipGeneration.Valid ||
		wait.OwnershipGeneration.Int64 != authority.workspace.OwnershipGeneration ||
		!wait.ParentWriterGeneration.Valid ||
		!wait.ChildWriterGeneration.Valid ||
		wait.ResumeWriterGeneration.Valid ||
		wait.ParentWriterGeneration.Int64 >= wait.ChildWriterGeneration.Int64 ||
		wait.ChildWriterGeneration.Int64 != authority.workspace.WriterGeneration ||
		wait.ChildWriterGeneration.Int64 != authority.workspaceLease.WriterGeneration ||
		authority.workspace.OwnerRunID.Valid == authority.workspace.OwnerSessionID.Valid ||
		authority.workspace.OwnerRunID == authority.run.ID {
		return errStaleRunFinalization
	}
	if authority.parentEnclosingWait.ID.Valid {
		if err := validateSameWorkspaceAncestors(
			authority.sameWorkspaceAncestors,
			authority.parentRun,
			wait,
		); err != nil {
			return errStaleRunFinalization
		}
	}
	return nil
}

func validateSameWorkspaceAncestors(
	ancestors []db.LockSameWorkspaceAncestorsRow,
	immediateParent db.Run,
	inner db.RunWait,
) error {
	if len(ancestors) == 0 {
		return errStaleRunFinalization
	}
	for index, row := range ancestors {
		wait := row.RunWait
		parent := row.Run
		attempt := row.RunAttempt
		if row.Depth != int32(len(ancestors)-index-1) ||
			wait.RunID != parent.ID ||
			wait.AttemptNumber != attempt.Number ||
			parent.CurrentAttemptNumber != attempt.Number ||
			parent.WorkspaceID != inner.WorkspaceID ||
			parent.Status != db.RunStatusWaiting ||
			parent.CurrentRunLeaseID.Valid ||
			attempt.TerminalAt.Valid ||
			wait.WorkspaceID != inner.WorkspaceID ||
			wait.OwnershipGeneration != inner.OwnershipGeneration ||
			!wait.ChildWriterGeneration.Valid ||
			wait.ResumeWriterGeneration.Valid {
			return errStaleRunFinalization
		}
		if index == 0 {
			continue
		}
		outer := ancestors[index-1].RunWait
		if parent.ID != outer.ChildRunID ||
			!outer.ChildWriterGeneration.Valid ||
			outer.ChildWriterGeneration != wait.ParentWriterGeneration {
			return errStaleRunFinalization
		}
	}
	closest := ancestors[len(ancestors)-1].RunWait
	if closest.ChildRunID != immediateParent.ID ||
		closest.ChildWriterGeneration != inner.ParentWriterGeneration {
		return errStaleRunFinalization
	}
	return nil
}

func staleRunFinalization(err error) error {
	if err == nil {
		return errStaleRunFinalization
	}
	return errors.Join(errStaleRunFinalization, err)
}
