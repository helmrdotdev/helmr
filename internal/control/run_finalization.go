package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
)

var errStaleRunFinalization = errors.New("Run finalization authority is stale")

type parsedRunFinalization struct {
	lease       parsedRunLeaseReceipt
	operationID uuid.UUID
	kind        api.WorkerRunFinalizationKind
	fingerprint string
}

func parseRunFinalization(request api.WorkerBeginRunFinalizationRequest) (parsedRunFinalization, error) {
	lease, err := parseRunLeaseReceipt(request.Lease)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	operationID, err := parseCanonicalUUID("operation_id", request.OperationID)
	if err != nil {
		return parsedRunFinalization{}, err
	}
	if request.Kind != api.WorkerRunFinalizationCapture && request.Kind != api.WorkerRunFinalizationReset {
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
	if quiescedRunID != lease.runID ||
		quiescedLeaseID != lease.leaseID ||
		request.ProgramQuiesced.AttemptNumber != request.Lease.AttemptNumber {
		return parsedRunFinalization{}, errors.New("program_quiesced does not match the Run Lease")
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.OperationID = operationID.String()
	normalized.ProgramQuiesced.RunID = quiescedRunID.String()
	normalized.ProgramQuiesced.RunLeaseID = quiescedLeaseID.String()
	fingerprint, err := terminalRequestFingerprint("run.finalization.begin.v0", normalized)
	if err != nil {
		return parsedRunFinalization{}, fmt.Errorf("fingerprint Run finalization: %w", err)
	}
	return parsedRunFinalization{
		lease: lease, operationID: operationID, kind: request.Kind, fingerprint: fingerprint,
	}, nil
}

func (s *Server) beginRunFinalization(
	ctx context.Context,
	worker workerActor,
	request api.WorkerBeginRunFinalizationRequest,
	parsed parsedRunFinalization,
) (api.WorkerBeginRunFinalizationResponse, error) {
	var response api.WorkerBeginRunFinalizationResponse
	err := s.inTx(ctx, func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(
			ctx,
			work.q,
			pgvalue.UUID(parsed.lease.runID),
			request.Lease.AttemptNumber,
			pgvalue.UUID(parsed.lease.workspaceID),
		); err != nil {
			return fmt.Errorf("lock Run finalization Secret authority: %w", err)
		}
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunFinalization(err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockLiveRunLeaseAuthority(
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
		authority.actor = owner.actor
		authority.parentRun = owner.parent
		if err := validateRunFinalizationOwner(authority, locators); err != nil {
			return err
		}
		if !authority.attempt.EntrypointEnteredAt.Valid {
			return errStaleRunFinalization
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease,
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
			response = api.WorkerBeginRunFinalizationResponse{
				Lease: current, OperationID: parsed.operationID.String(), Kind: parsed.kind,
				StartedAt: authority.runLease.FinalizationStartedAt.Time.UTC(),
			}
			return nil
		}
		if authority.runLease.State != db.RunLeaseStateRunning ||
			!authority.run.ActiveStartedAt.Valid ||
			authority.runLease.FinalizationOperationID.Valid ||
			authority.runLease.FinalizationKind.Valid ||
			authority.runLease.FinalizationStartedAt.Valid ||
			authority.runLease.FinalizationRequestFingerprint.Valid ||
			!equalRunLeaseReceipt(current, request.Lease) {
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
				err = errors.New("database Run finalization time is unavailable")
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
		expiresAt := now.Time.Add(s.runFinalizationTTL)
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
		frozen, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease,
			workspace: authority.workspace, workspaceMount: authority.workspaceMount,
			workspaceLease: authority.workspaceLease,
		})
		if err != nil {
			return err
		}
		response = api.WorkerBeginRunFinalizationResponse{
			Lease: frozen, OperationID: parsed.operationID.String(), Kind: parsed.kind,
			StartedAt: now.Time.UTC(),
		}
		return nil
	})
	return response, err
}

type runFinalizationOwner struct {
	actor  db.Actor
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
	case locators.ActorID.Valid && !locators.ParentRunID.Valid && !locators.ParentOwnsLifecycle.Valid:
		owner.actor, err = q.LockRunLeaseClaimActor(ctx, db.LockRunLeaseClaimActorParams{
			ID: locators.ActorID, WorkspaceID: locators.WorkspaceID,
		})
	case !locators.ActorID.Valid && locators.ParentRunID.Valid && locators.ParentOwnsLifecycle.Valid && locators.ParentOwnsLifecycle.Bool:
		owner.parent, err = q.LockRunFinalizationParentRun(ctx, db.LockRunFinalizationParentRunParams{
			ID: locators.ParentRunID, OrgID: locators.OrgID, ProjectID: locators.ProjectID,
			EnvironmentID: locators.EnvironmentID,
		})
	case !locators.ActorID.Valid &&
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

func validateRunFinalizationOwner(
	authority runLeaseClaimAuthority,
	locators db.GetLiveRunLeaseLocatorsRow,
) error {
	switch authority.run.EntrypointKind {
	case "actor":
		if !locators.ActorID.Valid || authority.run.ActorID != locators.ActorID ||
			authority.actor.ID != locators.ActorID || authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") ||
			authority.run.ParentRunID.Valid || authority.run.ParentOwnsLifecycle.Valid {
			return errStaleRunFinalization
		}
	case "task":
		if authority.run.ActorID.Valid || authority.run.ParentRunID != locators.ParentRunID ||
			authority.run.ParentOwnsLifecycle != locators.ParentOwnsLifecycle {
			return errStaleRunFinalization
		}
		if locators.ParentRunID.Valid && locators.ParentOwnsLifecycle.Bool {
			if authority.parentRun.ID != locators.ParentRunID ||
				authority.parentRun.WorkspaceID == authority.run.WorkspaceID ||
				(authority.parentRun.Status != db.RunStatusWaiting &&
					authority.parentRun.Status != db.RunStatusCancelRequested) {
				return errStaleRunFinalization
			}
		}
	default:
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
