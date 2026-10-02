package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/run"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultRunWaitIdleTimeout = 30 * time.Second
	maxRunWaitIdleTimeout     = time.Hour
	maxRunWaitDuration        = 365 * 24 * time.Hour
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
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run wait JSON: %w", err))
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
	case workerapi.RunWaitKindSessionInput:
		s.workerCreateSessionInputRunWait(w, r, request, identity)
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
	normalized := request
	normalized.Metadata = metadata
	normalized.Tags = tags
	parsed, worker, locators, current, err := s.loadRunWaitRegistrationAuthority(r.Context(), normalized.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	idleDefault, err := s.runWaitIdleDefault(r.Context(), current)
	if err != nil {
		writeError(w, err)
		return
	}
	timeoutAt, idleTimeout, err := runWaitDeadlines(request, idleDefault)
	if err != nil {
		writeError(w, badRequest(err))
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
	fingerprint, err := run.RequestFingerprint("worker.run-wait.create.v1", normalized)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("fingerprint token wait registration: %w", err)))
		return
	}
	waitID := identity.waitID
	resumeAttachID := identity.resumeAttachID
	actorCursor := pgtype.Int8{}
	if request.SessionSpeculativeInputSequence != nil {
		actorCursor = pgtype.Int8{Int64: *request.SessionSpeculativeInputSequence, Valid: true}
	}
	turnID, generation, err := run.ParseWaitTurn(request.TurnID, request.RunGeneration)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	registered, err := s.tokenWaits.RegisterWait(r.Context(), token.WaitRegistration{
		TurnID: turnID, RunGeneration: generation,
		TokenID: tokenID, WaitID: waitID,
		RunLeaseID: parsed.leaseID, LeaseSequence: request.Lease.LeaseSequence,
		WorkerGroupID: worker.GroupID, WorkerHostID: worker.HostID,
		WorkerEpoch: worker.Epoch, RequestFingerprint: fingerprint,
		SessionSpeculativeInputSequence: actorCursor,
		TimeoutAt:                       timeoutAt, IdleTimeoutMS: idleTimeout,
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
		ResumeAttachID: resumeAttachID.String(), ComputerInstanceID: pgvalue.UUIDString(locators.ComputerInstanceID),
		WorkerEpoch: worker.Epoch,
	}
	if registered.SuspensionStatus == db.RunWaitStatusReleased {
		response.ResolutionKind, response.Resolution, err = tokenWaitDecision(
			registered.ConditionStatus, registered.Result, registered.ReasonCode,
		)
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	}
	s.captureDrainingComputers(r.Context(), worker.HostID, pgvalue.MustUUIDValue(locators.ComputerInstanceID))
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerPollRunWait(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunWaitPollRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run wait poll JSON: %w", err))
		return
	}
	parsed, _, locators, err := s.loadRunWaitLeaseAuthority(r.Context(), request.Lease)
	if err != nil {
		writeError(w, err)
		return
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	wait, stopped, err := run.PollWait(r.Context(), s.db, run.WaitPollScope{
		RunID: locators.RunID, AttemptNumber: locators.AttemptNumber,
		ComputerID: locators.ComputerID, LeaseID: pgvalue.UUID(parsed.leaseID),
	}, pgvalue.UUID(waitID))
	if err != nil {
		writeError(w, runError(err, runWaitPollOperation))
		return
	}
	if stopped {
		writeJSON(w, http.StatusOK, workerapi.RunWaitPollResponse{
			RunID: pgvalue.UUIDString(locators.RunID), RunWaitID: waitID.String(),
			Status: workerapi.RunWaitPollStatusResumeRequested, ResumeKind: "cancelled",
			ResumePayload: []byte(`{"reason_code":"session_stopped"}`),
		})
		return
	}
	response := workerapi.RunWaitPollResponse{RunID: pgvalue.UUIDString(locators.RunID), RunWaitID: waitID.String()}
	switch wait.SuspensionStatus {
	case db.RunWaitStatusReleased:
		response.Status = workerapi.RunWaitPollStatusResumeRequested
		if wait.Kind == db.WaitKindSessionInput {
			response.ResumeKind, response.ResumePayload, err = sessionInputWaitDecision(wait)
		} else if wait.Kind == db.WaitKindTimer {
			response.ResumeKind, response.ResumePayload, err = timerWaitDecision(wait)
		} else if wait.Kind == db.WaitKindChild {
			response.ResumeKind, response.ResumePayload, err = childRunWaitDecision(wait)
		} else {
			response.ResumeKind, response.ResumePayload, err = tokenWaitDecision(
				wait.ConditionStatus, wait.ConditionResult, pgvalue.TextValue(wait.ConditionReasonCode),
			)
		}
		if err != nil {
			writeError(w, conflict(err))
			return
		}
	case db.RunWaitStatusHot, db.RunWaitStatusCheckpointing:
		// Physical suspension is driven by the whole-Instance desired state.
		// Each logical waiter keeps polling until its coordinator pauses it.
		response.Status = workerapi.RunWaitPollStatusWaiting
	default:
		response.Status = workerapi.RunWaitPollStatusTerminal
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerAcknowledgeRunWaitResume(w http.ResponseWriter, r *http.Request) {
	var request workerapi.RunWaitResumeAckRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker run wait resume acknowledgement JSON: %w", err))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	waitID, err := parseCanonicalUUID("run_wait_id", request.RunWaitID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	checkpointID, err := parseCanonicalUUID("checkpoint_id", request.CheckpointID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	wait, err := run.AcknowledgeWaitResume(r.Context(), s.tx, workerExecutionFence(worker, parsed, request.Lease), pgvalue.UUID(waitID), pgvalue.UUID(checkpointID))
	if err != nil {
		s.writeRunError(w, err, runWaitResumeOperation, worker, request.Lease)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.RunWaitResumeAckResponse{RunID: pgvalue.UUIDString(wait.RunID), RunWaitID: request.RunWaitID, CheckpointID: request.CheckpointID})
}

func (s *Server) loadRunWaitRegistrationAuthority(
	ctx context.Context,
	receipt workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workergroup.HostPrincipal, db.GetLiveRunLeaseLocatorsRow, db.Run, error) {
	parsed, worker, locators, err := s.loadRunWaitLeaseAuthority(ctx, receipt)
	if err != nil {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, err
	}
	run, err := s.db.GetRun(ctx, db.GetRunParams{EnvironmentID: locators.EnvironmentID, ID: locators.RunID})
	if err != nil || (run.Status != db.RunStatusRunning && run.Status != db.RunStatusWaiting) ||
		(run.EntrypointKind != "task" && run.EntrypointKind != "actor") ||
		(run.EntrypointKind == "task") != !run.SessionID.Valid || run.CurrentRunLeaseID != pgvalue.UUID(parsed.leaseID) {
		if err == nil {
			err = errors.New("run is not an active task or actor")
		}
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, db.GetLiveRunLeaseLocatorsRow{}, db.Run{}, conflict(err)
	}
	return parsed, worker, locators, run, nil
}

func childRunWaitDecision(wait db.RunWait) (string, json.RawMessage, error) {
	if wait.Kind != db.WaitKindChild || wait.ConditionStatus != db.WaitStatusCompleted ||
		wait.ConditionResult == nil || !json.Valid(wait.ConditionResult) {
		return "", nil, errors.New("child run wait decision is invalid")
	}
	return "completed", append(json.RawMessage(nil), wait.ConditionResult...), nil
}

func (s *Server) loadRunWaitLeaseAuthority(
	ctx context.Context,
	receipt workerapi.RunLeaseFence,
) (parsedRunLeaseFence, workergroup.HostPrincipal, db.GetLiveRunLeaseLocatorsRow, error) {
	parsed, err := parseRunLeaseFence(receipt)
	if err != nil {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, db.GetLiveRunLeaseLocatorsRow{}, badRequest(err)
	}
	worker := workerFromContext(ctx)
	locators, err := s.db.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: receipt.LeaseSequence,
		WorkerGroupID: pgvalue.UUID(worker.GroupID), WorkerHostID: pgvalue.UUID(worker.HostID),
		WorkerEpoch: worker.Epoch})
	if isNoRows(err) {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, db.GetLiveRunLeaseLocatorsRow{}, conflict(errors.New("worker run wait receipt is stale"))
	}
	if err != nil {
		return parsedRunLeaseFence{}, workergroup.HostPrincipal{}, db.GetLiveRunLeaseLocatorsRow{}, errors.New("load worker run wait authority")
	}
	return parsed, worker, locators, nil
}

func runWaitDeadlines(request workerapi.CreateRunWaitRequest, defaultIdleTimeout time.Duration) (pgtype.Timestamptz, pgtype.Int8, error) {
	now := time.Now().UTC()
	var timeoutAt pgtype.Timestamptz
	if request.TimeoutMS != nil {
		if *request.TimeoutMS <= 0 || *request.TimeoutMS > maxRunWaitDuration.Milliseconds() {
			return pgtype.Timestamptz{}, pgtype.Int8{},
				fmt.Errorf("timeout_ms must be between 1 and %d", maxRunWaitDuration.Milliseconds())
		}
		duration := time.Duration(*request.TimeoutMS) * time.Millisecond
		timeoutAt = pgvalue.Timestamptz(now.Add(duration))
	}
	idleDuration, err := runWaitIdleDuration(request.IdleTimeoutMS, defaultIdleTimeout)
	if err != nil {
		return pgtype.Timestamptz{}, pgtype.Int8{}, err
	}
	return timeoutAt, pgtype.Int8{Int64: idleDuration.Milliseconds(), Valid: true}, nil
}

func runWaitIdleDuration(value *int64, defaultIdleTimeout time.Duration) (time.Duration, error) {
	if value == nil {
		return defaultIdleTimeout, nil
	}
	if *value <= 0 || *value > maxRunWaitIdleTimeout.Milliseconds() {
		return 0, fmt.Errorf("idle_timeout_ms must be between 1 and %d", maxRunWaitIdleTimeout.Milliseconds())
	}
	return time.Duration(*value) * time.Millisecond, nil
}

func (s *Server) runWaitIdleDefault(ctx context.Context, run db.Run) (time.Duration, error) {
	if run.EntrypointKind != "actor" {
		return defaultRunWaitIdleTimeout, nil
	}
	definition, err := s.db.GetDeploymentDefinition(ctx, db.GetDeploymentDefinitionParams{
		EnvironmentID: run.EnvironmentID, DeploymentID: run.DeploymentID,
		Kind: run.EntrypointKind, DeclaredID: run.EntrypointDeclaredID,
	})
	if err != nil {
		return 0, errors.New("load actor wait declaration")
	}
	return actorWaitIdleTimeout(definition.Manifest)
}

func tokenWaitDecision(state db.WaitStatus, result json.RawMessage, reason string) (string, json.RawMessage, error) {
	if len(result) == 0 {
		result = json.RawMessage(`null`)
	}
	switch state {
	case db.WaitStatusCompleted:
		return "completed", result, nil
	case db.WaitStatusCancelled:
		if reason == "" {
			reason = "token_cancelled"
		}
		return "cancelled", json.RawMessage(fmt.Sprintf(`{"reason_code":%q}`, reason)), nil
	case db.WaitStatusFailed:
		if reason == "" {
			return "", nil, errors.New("failed run wait decision has no reason")
		}
		return "failed", json.RawMessage(fmt.Sprintf(`{"reason_code":%q}`, reason)), nil
	default:
		return "", nil, errors.New("run wait decision is not terminal")
	}
}
