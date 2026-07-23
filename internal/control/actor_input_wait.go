package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/helmrdotdev/helmr/internal/actorinput"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerActorInputWaitParams struct {
	ActorID            string `json:"actor_id"`
	AfterInputSequence int64  `json:"after_input_sequence"`
}

type actorWaitManifest struct {
	IdleTimeoutMS int64 `json:"idleTimeoutMs"`
}

func (s *Server) workerCreateActorInputRunWait(
	w http.ResponseWriter,
	r *http.Request,
	request api.WorkerCreateRunWaitRequest,
) {
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	var params workerActorInputWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Actor input Wait params: %w", err)))
		return
	}
	actorID, err := parseCanonicalUUID("params.actor_id", params.ActorID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if params.AfterInputSequence < 0 || request.ActorSpeculativeInputSequence == nil ||
		params.AfterInputSequence != *request.ActorSpeculativeInputSequence {
		writeError(w, badRequest(errors.New("Actor input Wait cursors must be present, non-negative, and equal")))
		return
	}
	metadata, tags, err := normalizeRunWaitPresentation(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	parsed, worker, _, run, err := s.loadRunWaitRegistrationAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	if run.EntrypointKind != "actor" || !run.ActorID.Valid || run.ActorID != pgvalue.UUID(actorID) {
		writeError(w, conflict(errors.New("Actor input Wait must target the owning Actor")))
		return
	}
	definition, err := s.db.GetDeploymentDefinition(r.Context(), db.GetDeploymentDefinitionParams{
		EnvironmentID: run.EnvironmentID, DeploymentID: run.DeploymentID,
		Kind: run.EntrypointKind, DeclaredID: run.EntrypointDeclaredID,
	})
	if err != nil {
		writeError(w, errors.New("load Actor input Wait declaration"))
		return
	}
	idleTimeoutDefault, err := actorInputWaitIdleTimeout(definition.Manifest)
	if err != nil {
		writeError(w, errors.New("Actor input Wait declaration is invalid"))
		return
	}
	timeoutAt, idleTimeout, checkpointDueAt, checkpointDelay, err := runWaitDeadlines(
		request, idleTimeoutDefault,
	)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	normalized := request
	normalized.Lease.StartDeadlineAt = request.Lease.StartDeadlineAt.UTC()
	normalized.Lease.ExpiresAt = request.Lease.ExpiresAt.UTC()
	normalized.Params, err = json.Marshal(workerActorInputWaitParams{
		ActorID: actorID.String(), AfterInputSequence: params.AfterInputSequence,
	})
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize Actor input Wait params: %w", err)))
		return
	}
	normalized.Metadata = metadata
	normalized.Tags = tags
	fingerprint, err := terminalRequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint Actor input Wait registration: %w", err)))
		return
	}
	waitID := derivedRunWaitID(parsed.runID, request.Lease.AttemptNumber, correlationID, "wait")
	resumeAttachID := derivedRunWaitID(parsed.runID, request.Lease.AttemptNumber, correlationID, "resume-attach")

	var registered db.RunWait
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := secret.LockAttemptDelivery(
			r.Context(), work.q, pgvalue.UUID(parsed.runID), request.Lease.AttemptNumber, pgvalue.UUID(parsed.workspaceID),
		); err != nil {
			return fmt.Errorf("lock Actor input Wait Secret authority: %w", err)
		}
		lockedLocators, err := work.q.GetLiveRunLeaseLocators(r.Context(), db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: request.Lease.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		owner, err := lockRunFinalizationOwner(r.Context(), work.q, lockedLocators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			r.Context(), work.q, worker, pgvalue.UUID(parsed.leaseID), request.Lease.LeaseSequence, lockedLocators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.run.ParentRunID.Valid || authority.run.EntrypointKind != "actor" ||
			authority.run.ActorID != pgvalue.UUID(actorID) || authority.runLease.State != db.RunLeaseStateRunning {
			return errStaleRunLeaseClaim
		}
		current, err := projectRunLeaseReceipt(runLeaseProjectionAuthority{
			run: authority.run, attempt: authority.attempt, runtime: authority.runtime,
			networkSlot: authority.networkSlot, runLease: authority.runLease, workspace: authority.workspace,
			workspaceMount: authority.workspaceMount, workspaceLease: authority.workspaceLease,
		})
		if err != nil || !equalCurrentOrPreviousRunLeaseReceipt(current, request.Lease, authority.runLease.PreviousExpiresAt) {
			return errStaleRunLeaseClaim
		}
		cursor := pgtype.Int8{Int64: params.AfterInputSequence, Valid: true}
		if err := validateRootRunWaitActorCursor(authority, db.RunWait{ActorSpeculativeInputSequence: cursor}); err != nil {
			return err
		}
		replayParams := db.GetActorInputRunWaitRegistrationReplayParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			WorkspaceID: authority.workspace.ID, ActorID: authority.actor.ID,
			AfterInputSequence: cursor, ActorSpeculativeInputSequence: cursor,
			AttemptNumber: authority.attempt.Number, ResumeAttachID: pgvalue.UUID(resumeAttachID),
			RegistrationRequestFingerprint: pgvalue.Text(fingerprint), Metadata: metadata, Tags: tags,
			RunLeaseID: authority.runLease.ID,
		}
		registered, err = work.q.GetActorInputRunWaitRegistrationReplay(r.Context(), replayParams)
		if err == nil {
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if existing, existingErr := work.q.GetRunWait(r.Context(), db.GetRunWaitParams{
			RunID: authority.run.ID, AttemptNumber: authority.attempt.Number, ID: pgvalue.UUID(waitID),
		}); existingErr == nil || !errors.Is(existingErr, pgx.ErrNoRows) {
			_ = existing
			return errStaleRunLeaseClaim
		}
		if authority.run.Status != db.RunStatusRunning {
			return errStaleRunLeaseClaim
		}
		registered, err = work.q.RegisterActorInputRunWait(r.Context(), db.RegisterActorInputRunWaitParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID, TimeoutAt: timeoutAt,
			IdleTimeoutMs: idleTimeout, ActorID: authority.actor.ID, AfterInputSequence: cursor,
			RegistrationRequestFingerprint: pgvalue.Text(fingerprint), AttemptNumber: authority.attempt.Number,
			ActorSpeculativeInputSequence: cursor, CurrentRunLeaseID: authority.runLease.ID,
			CheckpointDueAt: checkpointDueAt, ResumeAttachID: pgvalue.UUID(resumeAttachID), Metadata: metadata, Tags: tags,
			RunID: authority.run.ID, ExpectedRunningStateVersion: authority.run.StateVersion,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		record, err := work.q.GetActorInputRecordAtSequenceForUpdate(r.Context(), db.GetActorInputRecordAtSequenceForUpdateParams{
			EnvironmentID: authority.run.EnvironmentID, ActorID: authority.actor.ID,
			Sequence: params.AfterInputSequence + 1,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			if authority.actor.State == "closing" && authority.actor.CloseSequence.Valid &&
				params.AfterInputSequence >= authority.actor.CloseSequence.Int64 {
				registered, err = actorinput.FailWait(r.Context(), work.q, registered, "actor_closed")
				return err
			}
			return nil
		}
		if err != nil {
			return err
		}
		registered, err = actorinput.CompleteWait(r.Context(), work.q, registered, record)
		return err
	})
	if errors.Is(err, errStaleRunLeaseClaim) {
		writeError(w, conflict(errors.New("worker Actor input Wait receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("register worker Actor input Wait failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("register worker Actor input Wait"))
		return
	}
	response := api.WorkerCreateRunWaitResponse{
		RunID: request.Lease.RunID, RunWaitID: waitID.String(), ResumeAttachID: resumeAttachID.String(),
		RuntimeInstanceID: request.Lease.RuntimeInstanceID, RuntimeEpoch: request.Lease.WorkerEpoch,
		CheckpointDelayMs: checkpointDelay.Milliseconds(),
	}
	if registered.SuspensionState == db.RunWaitStateReleased {
		response.ResolutionKind, response.Resolution, err = actorInputWaitDecision(registered)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func actorInputWaitIdleTimeout(raw json.RawMessage) (time.Duration, error) {
	var manifest actorWaitManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return 0, err
	}
	if manifest.IdleTimeoutMS <= 0 || manifest.IdleTimeoutMS > maxTokenWaitIdleTimeout.Milliseconds() {
		return 0, errors.New("Actor idle timeout is outside the supported range")
	}
	return time.Duration(manifest.IdleTimeoutMS) * time.Millisecond, nil
}

func actorInputWaitDecision(wait db.RunWait) (string, json.RawMessage, error) {
	switch wait.ConditionState {
	case db.WaitStateCompleted:
		if !wait.CompletedActorRecordID.Valid || len(wait.ConditionResult) == 0 {
			return "", nil, errors.New("completed Actor input Wait is missing its record")
		}
		return "completed", wait.ConditionResult, nil
	case db.WaitStateFailed:
		reason := pgvalue.TextValue(wait.ConditionReasonCode)
		if reason != "wait_timeout" && reason != "actor_closed" {
			return "", nil, errors.New("Actor input Wait failure reason is invalid")
		}
		payload, _ := json.Marshal(map[string]string{"reason_code": reason})
		return "failed", payload, nil
	case db.WaitStateCancelled:
		payload, _ := json.Marshal(map[string]string{"reason_code": pgvalue.TextValue(wait.ConditionReasonCode)})
		return "cancelled", payload, nil
	default:
		return "", nil, errors.New("Actor input Wait is not terminal")
	}
}
