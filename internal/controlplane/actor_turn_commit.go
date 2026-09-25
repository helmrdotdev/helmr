package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
)

var errStaleActorTurnCommit = errors.New("actor turn commit is stale")

type parsedActorTurnCommit struct {
	turnID              uuid.UUID
	generation          int64
	disposition         string
	result              json.RawMessage
	fingerprint         string
	lease               parsedRunLeaseFence
	correlationID       uuid.UUID
	targetInputSequence int64
}

func parseActorTurnCommitRequest(request workerapi.CommitActorTurnRequest) (parsedActorTurnCommit, error) {
	turnID, err := parseCanonicalUUID("turn_id", request.TurnID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.RunGeneration <= 0 {
		return parsedActorTurnCommit{}, errors.New("run_generation must be positive")
	}
	if request.Disposition != "completed" && request.Disposition != "failed" {
		return parsedActorTurnCommit{}, errors.New("disposition must be completed or failed")
	}
	payload := request.Result
	if request.Disposition == "failed" {
		if len(request.Result) != 0 || len(request.Error) == 0 {
			return parsedActorTurnCommit{}, errors.New("failed settlement requires error and forbids result")
		}
		payload = request.Error
	} else if len(request.Error) != 0 {
		return parsedActorTurnCommit{}, errors.New("completed settlement forbids error")
	}
	var result json.RawMessage
	if len(payload) != 0 {
		result, err = canonicalJSON(payload)
		if err != nil {
			return parsedActorTurnCommit{}, errors.New("settlement payload must be valid JSON")
		}
	}
	if request.Disposition == "failed" {
		request.Error = result
	} else {
		request.Result = result
	}
	fingerprint, err := terminalRequestFingerprint("worker.turn.settle.v1", request)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	lease, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		return parsedActorTurnCommit{}, err
	}
	if request.TargetInputSequence <= 0 {
		return parsedActorTurnCommit{}, errors.New("target_input_sequence must be positive")
	}
	return parsedActorTurnCommit{
		turnID: turnID, generation: request.RunGeneration, disposition: request.Disposition, result: result, fingerprint: fingerprint,
		lease: lease, correlationID: correlationID, targetInputSequence: request.TargetInputSequence,
	}, nil
}

func (s *Server) commitActorTurn(
	ctx context.Context,
	worker workerActor,
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
) (workerapi.CommitActorTurnResponse, error) {
	var response workerapi.CommitActorTurnResponse
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(commit.lease.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleActorTurnCommit(err)
		}
		if _, err := secret.LockAttemptDelivery(
			ctx, work.q, locators.RunID, locators.AttemptNumber,
			locators.WorkspaceID,
		); err != nil {
			return fmt.Errorf("lock actor turn secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil || !owner.actor.ID.Valid {
			return staleActorTurnCommit(err)
		}
		authority, err := lockLiveRunLeaseAuthority(
			ctx, work.q, worker, pgvalue.UUID(commit.lease.leaseID), request.Lease.LeaseSequence, locators,
		)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		authority.actor = owner.actor
		if err := validateActorTurnAuthority(ctx, work.q, authority); err != nil {
			return err
		}

		if authority.actor.CommittedInputSequence == commit.targetInputSequence {
			var replayed bool
			response, replayed, err = replayActorTurnCommit(ctx, work.q, request, commit, authority)
			if err != nil {
				return err
			}
			if replayed {
				return nil
			}
			return errStaleActorTurnCommit
		}
		scope := session.TurnScope{EnvironmentID: pgvalue.MustUUIDValue(authority.run.EnvironmentID), SessionID: pgvalue.MustUUIDValue(authority.actor.ID), TurnID: commit.turnID, RunID: pgvalue.MustUUIDValue(authority.run.ID), AttemptNumber: authority.attempt.Number, RunGeneration: commit.generation}
		input, err := session.ValidateTurn(ctx, work.q, scope)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		if input.Sequence != commit.targetInputSequence {
			return errStaleActorTurnCommit
		}
		if authority.actor.CommittedInputSequence+1 != commit.targetInputSequence ||
			commit.targetInputSequence >= authority.actor.NextInputSequence {
			return errStaleActorTurnCommit
		}
		committedAt, err := work.q.GetTaskCompletionTime(ctx)
		if err != nil || !committedAt.Valid {
			if err == nil {
				err = errors.New("database actor turn commit time is unavailable")
			}
			return err
		}
		if !committedAt.Time.Before(authority.runLease.ExpiresAt.Time) ||
			!committedAt.Time.Before(authority.workspaceLease.ExpiresAt.Time) {
			return errStaleActorTurnCommit
		}
		event, err := session.SettleTurn(ctx, work.q, scope, commit.disposition, commit.result, commit.fingerprint)
		if err != nil {
			return staleActorTurnCommit(err)
		}
		response = projectActorTurnResponse(request, commit)
		response.EventID = pgvalue.UUIDString(event.ID)
		return nil
	})
	return response, err
}

func validateActorTurnAuthority(ctx context.Context, store db.Querier, authority runLeaseClaimAuthority) error {
	actor := authority.actor
	if authority.run.EntrypointKind != "actor" || !authority.run.SessionID.Valid ||
		authority.run.SessionID != actor.ID || authority.run.ParentRunID.Valid ||
		authority.run.ParentOwnsLifecycle.Valid || authority.runLease.Status != db.RunLeaseStatusRunning ||
		!authority.run.ActiveStartedAt.Valid || !authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid || !authority.attempt.SessionInputStartSequence.Valid ||
		!authority.run.SessionInputStartSequence.Valid || !authority.run.SessionInputHighWatermark.Valid ||
		!actor.CurrentRunID.Valid || actor.CurrentRunID != authority.run.ID ||
		(actor.Status != "open" && actor.Status != "closing") ||
		authority.workspace.OwnerSessionID != actor.ID || authority.workspace.OwnerRunID.Valid ||
		!authority.workspace.HeadVersionID.Valid ||
		authority.runLease.FinalizationOperationID.Valid ||
		authority.runLease.FinalizationStartedAt.Valid || authority.runLease.FinalizationRequestFingerprint.Valid {
		return errStaleActorTurnCommit
	}
	clear, err := store.RunFinalizationScopeIsClear(ctx, db.RunFinalizationScopeIsClearParams{
		RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
	})
	if err != nil {
		return err
	}
	if !clear.Valid || !clear.Bool {
		return errStaleActorTurnCommit
	}
	return nil
}

func replayActorTurnCommit(
	ctx context.Context,
	store db.Querier,
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
	authority runLeaseClaimAuthority,
) (workerapi.CommitActorTurnResponse, bool, error) {
	input, err := store.LockSessionTurnInput(ctx, db.LockSessionTurnInputParams{EnvironmentID: authority.run.EnvironmentID, SessionID: authority.actor.ID, ID: pgvalue.UUID(commit.turnID)})
	if err != nil {
		return workerapi.CommitActorTurnResponse{}, false, staleActorTurnCommit(err)
	}
	if input.Status != commit.disposition || input.TerminalRequestFingerprint.String != commit.fingerprint || input.RunID != authority.run.ID || input.AttemptNumber.Int32 != authority.attempt.Number || input.RunGeneration.Int64 != commit.generation || !input.TerminalEventID.Valid {
		return workerapi.CommitActorTurnResponse{}, false, nil
	}
	response := projectActorTurnResponse(request, commit)
	response.EventID = pgvalue.UUIDString(input.TerminalEventID)
	return response, true, nil
}

func projectActorTurnResponse(
	request workerapi.CommitActorTurnRequest,
	commit parsedActorTurnCommit,
) workerapi.CommitActorTurnResponse {
	return workerapi.CommitActorTurnResponse{
		Lease: request.Lease, CorrelationID: commit.correlationID.String(),
		CommittedInputSequence: commit.targetInputSequence,
	}
}

func staleActorTurnCommit(err error) error {
	var operation *session.OperationError
	if errors.As(err, &operation) {
		return errors.Join(errStaleActorTurnCommit, err)
	}
	if errors.Is(err, errStaleWorkerClaims) {
		return err
	}
	if err == nil || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errStaleRunLeaseClaim) ||
		errors.Is(err, errStaleRunFinalization) || errors.Is(err, errStaleActorCompletion) ||
		errors.Is(err, session.ErrTurnStopped) || errors.Is(err, session.ErrTurnNotActive) || errors.Is(err, session.ErrTurnScope) {
		return errStaleActorTurnCommit
	}
	return err
}
