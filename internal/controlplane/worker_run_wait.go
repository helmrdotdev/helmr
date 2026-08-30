package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	rootRunWaitHotWindow      = 2 * time.Minute
	defaultRunWaitIdleTimeout = 30 * time.Second
	maxRunWaitIdleTimeout     = time.Hour
	maxRunWaitDuration        = 365 * 24 * time.Hour
	shortWaitGrace            = time.Second
)

type workerTokenWaitParams struct {
	TokenID string `json:"token_id"`
}

type requestedRunWaitIdentity struct {
	correlationID  uuid.UUID
	waitID         uuid.UUID
	resumeAttachID uuid.UUID
}

func parseRequestedRunWaitIdentity(request workerapi.CreateRunWaitRequest) (requestedRunWaitIdentity, error) {
	correlationID, err := ids.Parse(request.CorrelationID)
	if err != nil {
		return requestedRunWaitIdentity{}, errors.New("correlation_id must be a canonical UUIDv7")
	}
	waitID, err := ids.Parse(request.RunWaitID)
	if err != nil {
		return requestedRunWaitIdentity{}, errors.New("run_wait_id must be a canonical UUIDv7")
	}
	resumeAttachID, err := ids.Parse(request.ResumeAttachID)
	if err != nil {
		return requestedRunWaitIdentity{}, errors.New("resume_attach_id must be a canonical UUIDv7")
	}
	if correlationID == waitID || correlationID == resumeAttachID || waitID == resumeAttachID {
		return requestedRunWaitIdentity{}, errors.New("correlation_id, run_wait_id, and resume_attach_id must be distinct")
	}
	return requestedRunWaitIdentity{
		correlationID: correlationID, waitID: waitID, resumeAttachID: resumeAttachID,
	}, nil
}

func (s *Server) workerCreateRunWait(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CreateRunWaitRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run wait JSON: %w", err)))
		return
	}
	identity, err := parseRequestedRunWaitIdentity(request)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	switch request.Kind {
	case workerapi.RunWaitKindToken:
		s.workerCreateTokenRunWait(w, r, request, identity)
	case workerapi.RunWaitKindTimer:
		s.workerCreateTimerRunWait(w, r, request, identity)
	case workerapi.RunWaitKindActorInput:
		s.workerCreateActorInputRunWait(w, r, request, identity)
	default:
		writeError(w, badRequest(fmt.Errorf("run wait kind %q is not implemented by the durable runtime", request.Kind)))
	}
}

func (s *Server) workerCreateTokenRunWait(
	w http.ResponseWriter,
	r *http.Request,
	request workerapi.CreateRunWaitRequest,
	identity requestedRunWaitIdentity,
) {
	var params workerTokenWaitParams
	if err := decodeClosedJSON(request.Params, &params); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token wait params: %w", err)))
		return
	}
	tokenID, err := ids.Parse(params.TokenID)
	if err != nil {
		writeError(w, badRequest(errors.New("params.token_id must be a token ID")))
		return
	}
	metadata, tags, err := normalizeWaitAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	timeoutAt, idleTimeout, checkpointDueAt, err := runWaitDeadlines(request, defaultRunWaitIdleTimeout)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	normalized := request
	normalized.Metadata = metadata
	normalized.Tags = tags
	parsed, worker, locators, _, err := s.loadRunWaitRegistrationAuthority(r.Context(), normalized.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	tokenRow, err := s.db.GetTokenByID(r.Context(), pgvalue.UUID(tokenID))
	if err != nil || tokenRow.EnvironmentID != locators.EnvironmentID {
		writeError(w, notFound(errTokenNotFound))
		return
	}
	tokenID = pgvalue.MustUUIDValue(tokenRow.ID)
	normalized.Params, err = json.Marshal(workerTokenWaitParams{TokenID: params.TokenID})
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("normalize token wait params: %w", err)))
		return
	}
	fingerprint, err := terminalRequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint token wait registration: %w", err)))
		return
	}
	waitID := identity.waitID
	resumeAttachID := identity.resumeAttachID
	reconcileDB, ok := s.tx.(token.WaitDB)
	if !ok {
		writeError(w, unavailable(errors.New("durable token wait storage is not configured")))
		return
	}
	reconciler, err := token.NewWaitReconciler(reconcileDB)
	if err != nil {
		writeError(w, unavailable(err))
		return
	}
	actorCursor := pgtype.Int8{}
	if request.ActorSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{Int64: *request.ActorSpeculativeInputSequence, Valid: true}
	}
	registered, err := reconciler.RegisterWait(r.Context(), token.WaitRegistration{
		TokenID: tokenID, WaitID: waitID, ResumeAttachID: resumeAttachID,
		RunLeaseID: parsed.leaseID, LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: worker.WorkerInstanceID,
		WorkerEpoch: worker.WorkerEpoch, RequestFingerprint: fingerprint,
		ActorSpeculativeInputSequence: actorCursor,
		TimeoutAt:                     timeoutAt, IdleTimeoutMS: idleTimeout, CheckpointDueAt: checkpointDueAt,
		Metadata: metadata, Tags: tags,
	})
	if errors.Is(err, token.ErrWaitAuthority) {
		writeError(w, conflict(errors.New("worker run wait receipt is stale")))
		return
	}
	if err != nil {
		s.log.Error("register worker Token Wait failed", "run_id", pgvalue.UUIDString(locators.RunID), "error", err)
		writeError(w, errors.New("register worker token wait"))
		return
	}
	response := workerapi.CreateRunWaitResponse{
		RunID: pgvalue.UUIDString(locators.RunID), RunWaitID: registered.WaitID.String(),
		ResumeAttachID: resumeAttachID.String(), RuntimeInstanceID: pgvalue.UUIDString(locators.RuntimeInstanceID),
		RuntimeEpoch: worker.WorkerEpoch,
	}
	if registered.SuspensionState == db.RunWaitStateReleased {
		response.ResolutionKind, response.Resolution, err = tokenWaitDecision(
			registered.ConditionState, registered.Result, registered.ReasonCode,
		)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	_ = worker
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerPollRunWait(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunWaitPollRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run wait poll JSON: %w", err)))
		return
	}
	parsed, worker, locators, err := s.loadRunWaitLeaseAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	wait, err := s.db.GetRunWait(r.Context(), db.GetRunWaitParams{
		RunID: locators.RunID, AttemptNumber: locators.AttemptNumber, ID: pgvalue.UUID(waitID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run wait is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker run wait"))
		return
	}
	if (wait.Kind != db.WaitKindToken && wait.Kind != db.WaitKindTimer && wait.Kind != db.WaitKindActorInput &&
		wait.Kind != db.WaitKindChild) ||
		wait.AttemptNumber != locators.AttemptNumber ||
		wait.WorkspaceID != locators.WorkspaceID ||
		(wait.CurrentRunLeaseID != pgvalue.UUID(parsed.leaseID) && wait.PriorRunLeaseID != pgvalue.UUID(parsed.leaseID)) {
		writeError(w, conflict(errors.New("worker run wait fence is stale")))
		return
	}
	response := workerapi.RunWaitPollResponse{RunID: pgvalue.UUIDString(locators.RunID), RunWaitID: waitID.String()}
	switch wait.SuspensionState {
	case db.RunWaitStateReleased:
		response.Status = workerapi.RunWaitPollStatusResumeRequested
		if wait.Kind == db.WaitKindActorInput {
			response.ResumeKind, response.ResumePayload, err = actorInputWaitDecision(wait)
		} else if wait.Kind == db.WaitKindTimer {
			response.ResumeKind, response.ResumePayload, err = timerWaitDecision(wait)
		} else if wait.Kind == db.WaitKindChild {
			response.ResumeKind, response.ResumePayload, err = childRunWaitDecision(wait)
		} else {
			response.ResumeKind, response.ResumePayload, err = tokenWaitDecision(
				wait.ConditionState, wait.ConditionResult, pgvalue.TextValue(wait.ConditionReasonCode),
			)
		}
		if err != nil {
			writeError(w, conflict(err))
			return
		}
		response.RequireAck = false
	case db.RunWaitStateHot:
		if wait.ConditionState != db.WaitStatePending {
			writeError(w, conflict(errors.New("terminal hot run wait was not released")))
			return
		}
		if wait.CheckpointDueAt.Valid && !time.Now().Before(wait.CheckpointDueAt.Time) {
			wait, err = s.requestWorkerRunWaitCheckpoint(r.Context(), worker, request.Lease, parsed, waitID)
			if err != nil {
				if errors.Is(err, errStaleRunLeaseClaim) || isNoRows(err) {
					writeError(w, conflict(errors.New("worker run wait checkpoint request is stale")))
				} else {
					writeError(w, errors.New("request worker run wait checkpoint"))
				}
				return
			}
		}
		if wait.SuspensionState == db.RunWaitStateHot {
			response.Status = workerapi.RunWaitPollStatusWaiting
			break
		}
		if !wait.SuspendCheckpointID.Valid || wait.CheckpointRequestVersion <= 0 {
			writeError(w, errors.New("checkpointing run wait has incomplete authority"))
			return
		}
		response.Status = workerapi.RunWaitPollStatusCheckpointRequested
		response.RequestVersion = wait.CheckpointRequestVersion
		response.CheckpointID = pgvalue.UUIDString(wait.SuspendCheckpointID)
		response.CaptureWorkspace = true
	case db.RunWaitStateCheckpointing:
		if !wait.SuspendCheckpointID.Valid || wait.CheckpointRequestVersion <= 0 {
			writeError(w, errors.New("checkpointing run wait has incomplete authority"))
			return
		}
		response.Status = workerapi.RunWaitPollStatusCheckpointRequested
		response.RequestVersion = wait.CheckpointRequestVersion
		response.CheckpointID = pgvalue.UUIDString(wait.SuspendCheckpointID)
		response.CaptureWorkspace = true
	default:
		response.Status = workerapi.RunWaitPollStatusTerminal
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerAcknowledgeRunWaitResume(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunWaitResumeAckRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run wait resume acknowledgement JSON: %w", err)))
		return
	}
	writeError(w, conflict(errors.New("hot run wait decisions do not require acknowledgement")))
}

func (s *Server) requestWorkerRunWaitCheckpoint(
	ctx context.Context,
	worker workerActor,
	receipt workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
	waitID uuid.UUID,
) (db.RunWait, error) {
	var updated db.RunWait
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := work.q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
			ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence,
			WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			WorkerEpoch: worker.WorkerEpoch})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if _, err := secret.LockAttemptDelivery(ctx, work.q, locators.RunID, locators.AttemptNumber, locators.WorkspaceID); err != nil {
			return fmt.Errorf("lock run wait secret authority: %w", err)
		}
		owner, err := lockRunFinalizationOwner(ctx, work.q, locators)
		if err != nil {
			return err
		}
		authority, err := lockRenewableRunLeaseAuthority(
			ctx, work.q, worker, pgvalue.UUID(parsed.leaseID), receipt.LeaseSequence, locators,
		)
		if err != nil {
			return err
		}
		authority.actor = owner.actor
		if authority.run.Status != db.RunStatusWaiting ||
			authority.runLease.State != db.RunLeaseStateRunning {
			return errStaleRunLeaseClaim
		}
		wait, err := work.q.LockRunLeaseClaimWait(ctx, db.LockRunLeaseClaimWaitParams{
			ID: pgvalue.UUID(waitID), EnvironmentID: authority.run.EnvironmentID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, WorkspaceID: authority.workspace.ID,
			CurrentRunLeaseID: authority.runLease.ID,
		})
		if err != nil {
			return staleRunLeaseClaim(err)
		}
		if err := validateRunWaitActorCursor(authority, wait); err != nil {
			return err
		}
		if wait.SuspensionState == db.RunWaitStateCheckpointing && wait.SuspendCheckpointID.Valid {
			updated = wait
			return nil
		}
		if (wait.Kind != db.WaitKindToken && wait.Kind != db.WaitKindActorInput &&
			wait.Kind != db.WaitKindChild) ||
			wait.ConditionState != db.WaitStatePending ||
			wait.SuspensionState != db.RunWaitStateHot || !wait.CheckpointDueAt.Valid {
			return errStaleRunLeaseClaim
		}
		checkpointID := pgvalue.UUID(uuid.NewV7())
		if _, err := work.q.CreateRunCheckpoint(ctx, db.CreateRunCheckpointParams{
			ID: checkpointID, RunID: authority.run.ID,
			AttemptNumber: authority.attempt.Number, RunWaitID: wait.ID,
			SourceRunLeaseID: authority.runLease.ID, SourceWorkspaceLeaseID: authority.workspaceLease.ID,
			WorkspaceID: authority.workspace.ID, BaseWorkspaceVersionID: authority.workspaceLease.BaseVersionID,
			ActorSpeculativeInputSequence: wait.ActorSpeculativeInputSequence,
			RestoreManifest:               []byte(`{}`),
		}); err != nil {
			return fmt.Errorf("create run checkpoint intent: %w", err)
		}
		if _, err := work.q.BeginRunLeaseCheckpoint(ctx, db.BeginRunLeaseCheckpointParams{
			ID: authority.runLease.ID, RunID: authority.run.ID, WorkspaceID: authority.workspace.ID,
			AttemptNumber: authority.attempt.Number, LeaseSequence: authority.runLease.LeaseSequence,
		}); err != nil {
			return staleRunLeaseClaim(err)
		}
		updated, err = work.q.RequestRunWaitCheckpoint(ctx, db.RequestRunWaitCheckpointParams{
			SuspendCheckpointID: checkpointID, RunID: authority.run.ID, AttemptNumber: authority.attempt.Number,
			ID: wait.ID, CurrentRunLeaseID: authority.runLease.ID,
		})
		return err
	})
	return updated, err
}

func (s *Server) loadRunWaitRegistrationAuthority(
	ctx context.Context,
	receipt workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workerActor, db.GetLiveRunLeaseLocatorsRow, db.Run, error) {
	parsed, worker, locators, err := s.loadRunWaitLeaseAuthority(ctx, receipt)
	if err != nil {
		return parsedRunLeaseFence{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, err
	}
	run, err := s.db.GetRun(ctx, db.GetRunParams{EnvironmentID: locators.EnvironmentID, ID: locators.RunID})
	if err != nil || (run.Status != db.RunStatusRunning && run.Status != db.RunStatusWaiting) ||
		(run.EntrypointKind != "task" && run.EntrypointKind != "actor") ||
		(run.EntrypointKind == "task") != !run.SessionID.Valid || run.CurrentRunLeaseID != pgvalue.UUID(parsed.leaseID) {
		if err == nil {
			err = errors.New("run is not an active task or actor")
		}
		return parsedRunLeaseFence{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, conflict(err)
	}
	return parsed, worker, locators, run, nil
}

func validateRunWaitActorCursor(authority runLeaseClaimAuthority, wait db.RunWait) error {
	switch authority.run.EntrypointKind {
	case "task":
		if authority.run.SessionID.Valid || wait.ActorSpeculativeInputSequence.Valid {
			return errStaleRunLeaseClaim
		}
	case "actor":
		cursor := wait.ActorSpeculativeInputSequence
		if !authority.run.SessionID.Valid || authority.run.SessionID != authority.actor.ID ||
			!authority.actor.CurrentRunID.Valid || authority.actor.CurrentRunID != authority.run.ID ||
			(authority.actor.State != "open" && authority.actor.State != "closing") ||
			!authority.attempt.SessionInputStartSequence.Valid || !cursor.Valid ||
			authority.attempt.SessionInputStartSequence.Int64 > authority.actor.CommittedInputSequence ||
			cursor.Int64 < authority.actor.CommittedInputSequence ||
			cursor.Int64 > authority.actor.CommittedInputSequence+1 ||
			cursor.Int64 >= authority.actor.NextInputSequence ||
			authority.workspace.OwnerSessionID != authority.actor.ID || authority.workspace.OwnerRunID.Valid {
			return errStaleRunLeaseClaim
		}
	default:
		return errStaleRunLeaseClaim
	}
	return nil
}

func childRunWaitDecision(wait db.RunWait) (string, json.RawMessage, error) {
	if wait.Kind != db.WaitKindChild || wait.ConditionState != db.WaitStateCompleted ||
		wait.ConditionResult == nil || !json.Valid(wait.ConditionResult) {
		return "", nil, errors.New("child run wait decision is invalid")
	}
	return "completed", append(json.RawMessage(nil), wait.ConditionResult...), nil
}

func (s *Server) loadRunWaitLeaseAuthority(
	ctx context.Context,
	receipt workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workerActor, db.GetLiveRunLeaseLocatorsRow, error) {
	parsed, err := parseRunLeaseFence(receipt)
	if err != nil {
		return parsedRunLeaseFence{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, badRequest(err)
	}
	worker := workerFromContext(ctx)
	locators, err := s.db.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence,
		WorkerGroupID: worker.WorkerGroupID, WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch: worker.WorkerEpoch})
	if isNoRows(err) {
		return parsedRunLeaseFence{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, conflict(errors.New("worker run wait receipt is stale"))
	}
	if err != nil {
		return parsedRunLeaseFence{}, workerActor{}, db.GetLiveRunLeaseLocatorsRow{}, errors.New("load worker run wait authority")
	}
	return parsed, worker, locators, nil
}

func runWaitDeadlines(request workerapi.CreateRunWaitRequest, defaultIdleTimeout time.Duration) (pgtype.Timestamptz, pgtype.Int8, pgtype.Timestamptz, error) {
	now := time.Now().UTC()
	checkpointDelay := rootRunWaitHotWindow
	var timeoutAt pgtype.Timestamptz
	if request.TimeoutMS != nil {
		if *request.TimeoutMS <= 0 || *request.TimeoutMS > maxRunWaitDuration.Milliseconds() {
			return pgtype.Timestamptz{}, pgtype.Int8{}, pgtype.Timestamptz{},
				fmt.Errorf("timeout_ms must be between 1 and %d", maxRunWaitDuration.Milliseconds())
		}
		duration := time.Duration(*request.TimeoutMS) * time.Millisecond
		timeoutAt = pgvalue.Timestamptz(now.Add(duration))
		if duration <= checkpointDelay {
			checkpointDelay = duration + shortWaitGrace
		}
	}
	idleDuration := defaultIdleTimeout
	if request.IdleTimeoutMS != nil {
		if *request.IdleTimeoutMS <= 0 || *request.IdleTimeoutMS > maxRunWaitIdleTimeout.Milliseconds() {
			return pgtype.Timestamptz{}, pgtype.Int8{}, pgtype.Timestamptz{},
				fmt.Errorf("idle_timeout_ms must be between 1 and %d", maxRunWaitIdleTimeout.Milliseconds())
		}
		idleDuration = time.Duration(*request.IdleTimeoutMS) * time.Millisecond
	}
	idleTimeout := pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true}
	if idleDuration < checkpointDelay {
		checkpointDelay = idleDuration
	}
	return timeoutAt, idleTimeout, pgvalue.Timestamptz(now.Add(checkpointDelay)), nil
}

func tokenWaitDecision(state db.WaitState, result json.RawMessage, reason string) (string, json.RawMessage, error) {
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	switch state {
	case db.WaitStateCompleted:
		return "completed", result, nil
	case db.WaitStateCancelled:
		if reason == "" {
			reason = "token_cancelled"
		}
		return "cancelled", json.RawMessage(fmt.Sprintf(`{"reason_code":%q}`, reason)), nil
	case db.WaitStateFailed:
		if reason == "" {
			return "", nil, errors.New("failed run wait decision has no reason")
		}
		return "failed", json.RawMessage(fmt.Sprintf(`{"reason_code":%q}`, reason)), nil
	default:
		return "", nil, errors.New("run wait decision is not terminal")
	}
}

func decodeClosedWorkerRequest(r *http.Request, destination any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is required")
		}
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func decodeClosedJSON(raw json.RawMessage, destination any) error {
	if len(raw) == 0 {
		return errors.New("value is required")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
