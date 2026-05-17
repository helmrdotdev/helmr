package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/control"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workerLeaseDuration   = 5 * time.Minute
	defaultWorkerTokenTTL = 15 * time.Minute
)

func (s *Server) workerRegister(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker registration storage is not configured"))
		return
	}
	if len(s.authSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker registration is not configured"))
		return
	}
	var request api.WorkerRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker registration request JSON: %w", err))
		return
	}
	registrationHash, err := auth.HashToken(s.authSecret, request.RegistrationToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker pool registration token is required"))
		return
	}
	generated, err := auth.GenerateWorkerSecret(s.authSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate worker credential"))
		return
	}
	workerID := "worker-" + ids.New().String()
	credential, err := s.db.CreateWorkerCredentialFromRegistration(r.Context(), db.CreateWorkerCredentialFromRegistrationParams{
		RegistrationTokenHash: registrationHash,
		CredentialID:          ids.ToPG(ids.New()),
		WorkerID:              workerID,
		KeyPrefix:             generated.KeyPrefix,
		SecretHash:            generated.TokenHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker pool registration token is invalid"))
		return
	}
	if err != nil {
		s.log.Error("worker registration failed", "worker_id", workerID, "resource_name", strings.TrimSpace(request.ResourceName), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("register worker"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerRegisterResponse{
		WorkerID:     credential.WorkerID,
		WorkerSecret: generated.Raw,
	})
}

func (s *Server) revokeWorkerCredentials(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker credential storage is not configured"))
		return
	}
	workerID := strings.TrimSpace(chi.URLParam(r, "workerID"))
	if workerID == "" {
		writeError(w, http.StatusBadRequest, errors.New("worker_id is required"))
		return
	}
	actor := actorFromContext(r.Context())
	rows, err := s.db.RevokeWorkerCredentialsByWorkerID(r.Context(), db.RevokeWorkerCredentialsByWorkerIDParams{
		OrgID:    ids.ToPG(actor.OrgID),
		WorkerID: workerID,
	})
	if err != nil {
		s.log.Error("revoke worker credentials failed", "worker_id", workerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("revoke worker credentials"))
		return
	}
	writeJSON(w, http.StatusOK, api.RevokeWorkerCredentialsResponse{Revoked: rows})
}

func (s *Server) workerAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || len(s.authSecret) == 0 || len(s.workerTokenSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker authentication is not configured"))
		return
	}
	var request api.WorkerTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker token request JSON: %w", err))
		return
	}
	request.WorkerID = strings.TrimSpace(request.WorkerID)
	if request.WorkerID == "" {
		writeError(w, http.StatusBadRequest, errors.New("worker_id is required"))
		return
	}
	secretHash, err := auth.HashToken(s.authSecret, request.WorkerSecret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	credential, err := s.db.AuthenticateWorkerCredential(r.Context(), db.AuthenticateWorkerCredentialParams{
		WorkerID:   request.WorkerID,
		SecretHash: secretHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	if err != nil {
		s.log.Error("worker credential authentication failed", "worker_id", request.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("worker authentication"))
		return
	}
	credentialID, err := ids.FromPG(credential.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("worker credential id"))
		return
	}
	orgID, err := ids.FromPG(credential.OrgID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("worker credential org id"))
		return
	}
	now := time.Now()
	expiresAt := now.Add(s.workerTokenTTL)
	signed, err := auth.IssueWorkerToken(s.workerTokenSecret, auth.WorkerClaims{
		OrgID:        orgID.String(),
		WorkerID:     credential.WorkerID,
		CredentialID: credentialID.String(),
		IssuedAt:     now,
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		s.log.Error("mint worker token failed", "worker_id", request.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerTokenResponse{
		Token:            signed,
		ExpiresInSeconds: int64(s.workerTokenTTL / time.Second),
	})
}

func (s *Server) workerClaim(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.github == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github resolver is not configured"))
		return
	}
	var request api.WorkerClaimRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker claim request JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	worker := workerFromContext(r.Context())
	if err := control.SweepOnceForOrg(r.Context(), s.db, ids.ToPG(worker.OrgID)); err != nil {
		s.log.Warn("sweep expired executions failed", "error", err)
	}
	if _, err := s.db.RefreshScopedWorkerHeartbeat(r.Context(), db.RefreshScopedWorkerHeartbeatParams{
		OrgID:          ids.ToPG(worker.OrgID),
		WorkerPoolID:   ids.ToPG(worker.WorkerPoolID),
		ID:             worker.WorkerID,
		RuntimeArch:    capabilities.RuntimeArch,
		RuntimeABI:     capabilities.RuntimeABI,
		KernelDigest:   capabilities.KernelDigest,
		RootfsDigest:   capabilities.RootfsDigest,
		CniProfile:     capabilities.CNIProfile,
		MaxVcpus:       int32(capabilities.MaxVCPUs),
		MaxMemoryMib:   int32(capabilities.MaxMemoryMiB),
		SlotsAvailable: capabilities.SlotsAvailable,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker is not active"))
		return
	} else if err != nil {
		s.log.Error("worker heartbeat failed", "worker_id", worker.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("record worker heartbeat"))
		return
	}
	claimed, err := s.db.ClaimRunExecution(r.Context(), db.ClaimRunExecutionParams{
		OrgID:          ids.ToPG(worker.OrgID),
		WorkerPoolID:   ids.ToPG(worker.WorkerPoolID),
		ExecutionID:    ids.ToPG(ids.New()),
		WorkerID:       worker.WorkerID,
		LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(workerLeaseDuration), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker claim failed", "worker_id", worker.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("claim run"))
		return
	}

	claim := workerClaimResponse(claimed)
	run, err := s.workerRunFromClaim(r.Context(), worker, claimed)
	if err != nil {
		if failure, ok := terminalPayloadFailure(err); ok {
			if failErr := s.failClaimedRunPayload(r.Context(), claimed, failure); failErr != nil {
				s.log.Error("fail worker run payload failed", "run_id", ids.MustFromPG(claimed.ID).String(), "execution_id", ids.MustFromPG(claimed.ExecutionID).String(), "error", failErr)
				writeError(w, http.StatusInternalServerError, errors.New("fail worker run payload"))
				return
			}
			s.log.Warn("terminal worker run payload failed", "run_id", ids.MustFromPG(claimed.ID).String(), "execution_id", ids.MustFromPG(claimed.ExecutionID).String(), "failure_kind", failure.kind, "error", err)
			writeJSON(w, http.StatusOK, api.WorkerClaimResponse{})
			return
		}
		if abandonErr := s.db.AbandonClaimedRunExecution(r.Context(), db.AbandonClaimedRunExecutionParams{
			OrgID:        claimed.OrgID,
			RunID:        claimed.ID,
			ExecutionID:  claimed.ExecutionID,
			WorkerPoolID: claimed.ExecutionWorkerPoolID,
			WorkerID:     claimed.ExecutionWorkerID,
		}); abandonErr != nil {
			s.log.Error("abandon worker claim failed", "run_id", ids.MustFromPG(claimed.ID).String(), "execution_id", ids.MustFromPG(claimed.ExecutionID).String(), "error", abandonErr)
		}
		s.log.Error("build worker run payload failed", "run_id", ids.MustFromPG(claimed.ID).String(), "execution_id", ids.MustFromPG(claimed.ExecutionID).String(), "error", err)
		writeError(w, http.StatusBadGateway, errors.New("build worker run payload"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerClaimResponse{Claim: &claim, Run: &run})
}

func (s *Server) workerActivate(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerActivateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker activate request JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.ActivateScopedWorkerHeartbeat(r.Context(), db.ActivateScopedWorkerHeartbeatParams{
		OrgID:          ids.ToPG(worker.OrgID),
		WorkerPoolID:   ids.ToPG(worker.WorkerPoolID),
		ID:             worker.WorkerID,
		RuntimeArch:    capabilities.RuntimeArch,
		RuntimeABI:     capabilities.RuntimeABI,
		KernelDigest:   capabilities.KernelDigest,
		RootfsDigest:   capabilities.RootfsDigest,
		CniProfile:     capabilities.CNIProfile,
		MaxVcpus:       int32(capabilities.MaxVCPUs),
		MaxMemoryMib:   int32(capabilities.MaxMemoryMiB),
		SlotsAvailable: capabilities.SlotsAvailable,
	}); err != nil {
		s.log.Error("worker activate failed", "worker_id", worker.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("activate worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.DrainWorker(r.Context(), db.DrainWorkerParams{
		OrgID: ids.ToPG(worker.OrgID),
		ID:    worker.WorkerID,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	} else if err != nil {
		s.log.Error("worker drain failed", "worker_id", worker.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("drain worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	s.writeWorkerStatus(w, r, workerFromContext(r.Context()))
}

func (s *Server) writeWorkerStatus(w http.ResponseWriter, r *http.Request, worker workerActor) {
	state, err := s.db.GetWorkerState(r.Context(), db.GetWorkerStateParams{
		OrgID: ids.ToPG(worker.OrgID),
		ID:    worker.WorkerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	}
	if err != nil {
		s.log.Error("get worker status failed", "worker_id", worker.WorkerID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker status"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStatusResponse{
		WorkerID:         state.ID,
		Status:           api.WorkerStatus(state.Status),
		ActiveExecutions: state.ActiveExecutions,
	})
}

func (s *Server) workerStart(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerStartRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker start request JSON: %w", err))
		return
	}
	claimIDs, err := parseWorkerClaim(request.Claim)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Claim.WorkerID != worker.WorkerID {
		writeError(w, http.StatusForbidden, errors.New("worker claim belongs to another worker"))
		return
	}
	status, err := s.db.StartRunExecution(r.Context(), db.StartRunExecutionParams{
		OrgID:        ids.ToPG(worker.OrgID),
		RunID:        ids.ToPG(claimIDs.runID),
		ExecutionID:  ids.ToPG(claimIDs.executionID),
		WorkerPoolID: ids.ToPG(worker.WorkerPoolID),
		WorkerID:     worker.WorkerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker claim is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker start failed", "run_id", request.Claim.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Claim.RunID, Status: string(status)})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker renew request JSON: %w", err))
		return
	}
	claimIDs, err := parseWorkerClaim(request.Claim)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Claim.WorkerID != worker.WorkerID {
		writeError(w, http.StatusForbidden, errors.New("worker claim belongs to another worker"))
		return
	}
	renewed, err := s.db.RenewRunExecutionLease(r.Context(), db.RenewRunExecutionLeaseParams{
		OrgID:          ids.ToPG(worker.OrgID),
		RunID:          ids.ToPG(claimIDs.runID),
		ExecutionID:    ids.ToPG(claimIDs.executionID),
		WorkerPoolID:   ids.ToPG(worker.WorkerPoolID),
		WorkerID:       worker.WorkerID,
		LeaseExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(workerLeaseDuration), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker claim is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker renew failed", "run_id", request.Claim.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("renew run execution"))
		return
	}
	claim := api.WorkerClaim{
		ID:        request.Claim.ID,
		RunID:     request.Claim.RunID,
		WorkerID:  renewed.WorkerID,
		ExpiresAt: pgTime(renewed.LeaseExpiresAt),
	}
	writeJSON(w, http.StatusOK, api.WorkerRenewResponse{Claim: claim})
}

func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	var request api.WorkerReleaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker release request JSON: %w", err))
		return
	}
	claimIDs, err := parseWorkerClaim(request.Claim)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Claim.WorkerID != worker.WorkerID {
		writeError(w, http.StatusForbidden, errors.New("worker claim belongs to another worker"))
		return
	}
	status, exitCode, errorMessage, err := releaseFields(request.Result)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	output := releaseOutput(request.Result, status, exitCode)
	terminalEventKind, terminalEventPayload, err := terminalRunEventForFields(status, exitCode, errorMessage, request.Result)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode terminal run event"))
		return
	}
	run, err := s.db.ReleaseRunExecution(r.Context(), db.ReleaseRunExecutionParams{
		OrgID:                ids.ToPG(worker.OrgID),
		RunID:                ids.ToPG(claimIDs.runID),
		ExecutionID:          ids.ToPG(claimIDs.executionID),
		WorkerPoolID:         ids.ToPG(worker.WorkerPoolID),
		WorkerID:             worker.WorkerID,
		Status:               status,
		ExitCode:             exitCode,
		Output:               output,
		ErrorMessage:         errorMessage,
		TerminalEventKind:    terminalEventKind,
		TerminalEventPayload: terminalEventPayload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker claim is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker release failed", "run_id", request.Claim.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("release run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Claim.RunID, Status: string(run.Status)})
}

func (s *Server) workerAppendLogs(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAppendLogRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker log request JSON: %w", err))
		return
	}
	content, err := base64.StdEncoding.DecodeString(request.ContentBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("log content is not valid base64"))
		return
	}
	kind := "log.stdout"
	switch request.Stream {
	case api.WorkerLogStreamStdout:
	case api.WorkerLogStreamStderr:
		kind = "log.stderr"
	default:
		writeError(w, http.StatusBadRequest, errors.New("stream must be stdout or stderr"))
		return
	}
	if request.ObservedSeq > uint64(^uint64(0)>>1) {
		writeError(w, http.StatusBadRequest, errors.New("observed_seq is too large"))
		return
	}
	worker, claimIDs, ok := s.workerClaimForWrite(w, r, request.Claim)
	if !ok {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":       request.Claim.RunID,
		"stream":       request.Stream,
		"observed_seq": request.ObservedSeq,
		"bytes":        len(content),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker log event"))
		return
	}
	_, err = s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:        ids.ToPG(worker.OrgID),
		RunID:        ids.ToPG(claimIDs.runID),
		ExecutionID:  ids.ToPG(claimIDs.executionID),
		WorkerPoolID: ids.ToPG(worker.WorkerPoolID),
		WorkerID:     worker.WorkerID,
		Stream:       db.RunLogStream(request.Stream),
		ObservedSeq:  int64(request.ObservedSeq),
		Content:      content,
		Kind:         kind,
		Payload:      payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker claim is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Claim.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Claim.RunID})
}

func (s *Server) workerRecordLogEntry(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRecordLogEntryRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker log entry request JSON: %w", err))
		return
	}
	payload, err := json.Marshal(map[string]string{"message": request.Entry})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker log entry"))
		return
	}
	s.appendWorkerEvent(w, r, request.Claim, "log", payload)
}

func (s *Server) workerEmitEvent(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerEmitEventRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker event request JSON: %w", err))
		return
	}
	request.EventType = strings.TrimSpace(request.EventType)
	if request.EventType == "" {
		writeError(w, http.StatusBadRequest, errors.New("event_type is required"))
		return
	}
	content := request.Content
	if len(content) == 0 {
		content = json.RawMessage(`null`)
	}
	if !json.Valid(content) {
		writeError(w, http.StatusBadRequest, errors.New("content must be valid JSON"))
		return
	}
	payload, err := json.Marshal(map[string]json.RawMessage{
		"type":    json.RawMessage(jsonString(request.EventType)),
		"content": content,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker event"))
		return
	}
	s.appendWorkerEvent(w, r, request.Claim, "emit."+request.EventType, payload)
}

func (s *Server) appendWorkerEvent(w http.ResponseWriter, r *http.Request, claim api.WorkerClaim, kind string, payload []byte) {
	worker, claimIDs, ok := s.workerClaimForWrite(w, r, claim)
	if !ok {
		return
	}
	_, err := s.db.AppendRunEventForExecution(r.Context(), db.AppendRunEventForExecutionParams{
		OrgID:        ids.ToPG(worker.OrgID),
		RunID:        ids.ToPG(claimIDs.runID),
		ExecutionID:  ids.ToPG(claimIDs.executionID),
		WorkerPoolID: ids.ToPG(worker.WorkerPoolID),
		WorkerID:     worker.WorkerID,
		Kind:         kind,
		Payload:      payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker claim is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker event failed", "run_id", claim.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker event"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: claim.RunID})
}

func (s *Server) workerClaimForWrite(w http.ResponseWriter, r *http.Request, claim api.WorkerClaim) (workerActor, workerClaimIDs, bool) {
	claimIDs, err := parseWorkerClaim(claim)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return workerActor{}, workerClaimIDs{}, false
	}
	worker := workerFromContext(r.Context())
	if claim.WorkerID != worker.WorkerID {
		writeError(w, http.StatusForbidden, errors.New("worker claim belongs to another worker"))
		return workerActor{}, workerClaimIDs{}, false
	}
	return worker, claimIDs, true
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

type payloadFailure struct {
	kind    string
	message string
}

type terminalPayloadError struct {
	kind string
	err  error
}

func (e terminalPayloadError) Error() string {
	return e.err.Error()
}

func (e terminalPayloadError) Unwrap() error {
	return e.err
}

func terminalPayload(kind string, err error) error {
	return terminalPayloadError{kind: kind, err: err}
}

func terminalPayloadFailure(err error) (payloadFailure, bool) {
	var terminal terminalPayloadError
	if !errors.As(err, &terminal) {
		return payloadFailure{}, false
	}
	return payloadFailure{kind: terminal.kind, message: terminal.err.Error()}, true
}

func (s *Server) failClaimedRunPayload(ctx context.Context, row db.ClaimRunExecutionRow, failure payloadFailure) error {
	kind, payload, err := payloadFailureRunEvent(failure)
	if err != nil {
		return err
	}
	_, err = s.db.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                row.OrgID,
		RunID:                row.ID,
		ExecutionID:          row.ExecutionID,
		WorkerPoolID:         row.ExecutionWorkerPoolID,
		WorkerID:             row.ExecutionWorkerID,
		Status:               db.RunStatusFailed,
		ExitCode:             pgtype.Int4{},
		ErrorMessage:         pgtype.Text{String: failure.message, Valid: true},
		TerminalEventKind:    kind,
		TerminalEventPayload: payload,
	})
	if err != nil {
		return err
	}
	return nil
}

func payloadFailureRunEvent(failure payloadFailure) (string, []byte, error) {
	payload, err := json.Marshal(map[string]any{
		"failure_kind": failure.kind,
		"detail":       map[string]any{"message": failure.message},
	})
	if err != nil {
		return "", nil, err
	}
	return "run.failed", payload, nil
}

type workerClaimIDs struct {
	executionID uuid.UUID
	runID       uuid.UUID
}

func parseWorkerClaim(claim api.WorkerClaim) (workerClaimIDs, error) {
	if strings.TrimSpace(claim.WorkerID) == "" {
		return workerClaimIDs{}, errors.New("claim.worker_id is required")
	}
	executionID, err := ids.Parse(claim.ID)
	if err != nil {
		return workerClaimIDs{}, errors.New("claim.id must be a UUID")
	}
	runID, err := ids.Parse(claim.RunID)
	if err != nil {
		return workerClaimIDs{}, errors.New("claim.run_id must be a UUID")
	}
	return workerClaimIDs{executionID: executionID, runID: runID}, nil
}

func releaseFields(result api.WorkerReleaseResult) (db.RunStatus, pgtype.Int4, pgtype.Text, error) {
	switch result.Kind {
	case "completed":
		if result.ExitCode == nil {
			return "", pgtype.Int4{}, pgtype.Text{}, errors.New("result.exit_code is required for completed releases")
		}
		status := db.RunStatusSucceeded
		if *result.ExitCode != 0 {
			status = db.RunStatusFailed
		}
		return status, pgtype.Int4{Int32: *result.ExitCode, Valid: true}, pgtype.Text{}, nil
	case "failed":
		message := "worker execution failed"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusFailed, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	case "cancelled":
		message := "worker execution cancelled"
		if result.Error != nil && *result.Error != "" {
			message = *result.Error
		}
		return db.RunStatusCancelled, pgtype.Int4{}, pgtype.Text{String: message, Valid: true}, nil
	default:
		return "", pgtype.Int4{}, pgtype.Text{}, fmt.Errorf("unsupported release result kind %q", result.Kind)
	}
}

func releaseOutput(result api.WorkerReleaseResult, status db.RunStatus, exitCode pgtype.Int4) []byte {
	if status != db.RunStatusSucceeded || !exitCode.Valid || exitCode.Int32 != 0 || len(result.Output) == 0 {
		return nil
	}
	return append([]byte(nil), result.Output...)
}

func terminalRunEvent(run db.ReleaseRunExecutionRow, result api.WorkerReleaseResult) (string, []byte, error) {
	return terminalRunEventForFields(run.Status, run.ExitCode, run.ErrorMessage, result)
}

func terminalRunEventForFields(status db.RunStatus, exitCode pgtype.Int4, errorMessage pgtype.Text, result api.WorkerReleaseResult) (string, []byte, error) {
	switch status {
	case db.RunStatusSucceeded:
		code := int32(0)
		if exitCode.Valid {
			code = exitCode.Int32
		}
		payload, err := json.Marshal(map[string]any{"exit_code": code})
		return "run.completed", payload, err
	case db.RunStatusFailed:
		if exitCode.Valid {
			payload, err := json.Marshal(map[string]any{
				"failure_kind": "task_failed",
				"detail":       map[string]any{"exit_code": exitCode.Int32},
			})
			return "run.failed", payload, err
		}
		message := "worker execution failed"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			message = errorMessage.String
		}
		if failureKind, ok := trustedWorkerFailureKind(result); ok {
			detail := map[string]any{"message": message}
			if result.LimitSeconds != nil {
				detail["limit_seconds"] = *result.LimitSeconds
			}
			payload, err := json.Marshal(map[string]any{
				"failure_kind": failureKind,
				"detail":       detail,
			})
			return "run.failed", payload, err
		}
		payload, err := json.Marshal(map[string]any{
			"failure_kind": "worker_failed",
			"detail":       map[string]any{"message": message},
		})
		return "run.failed", payload, err
	case db.RunStatusCancelled:
		reason := "worker execution cancelled"
		if errorMessage.Valid && strings.TrimSpace(errorMessage.String) != "" {
			reason = errorMessage.String
		}
		payload, err := json.Marshal(map[string]any{"reason": reason})
		return "run.cancelled", payload, err
	default:
		return "", nil, fmt.Errorf("run status %q is not terminal", status)
	}
}

func trustedWorkerFailureKind(result api.WorkerReleaseResult) (string, bool) {
	if result.FailureKind == nil {
		return "", false
	}
	switch *result.FailureKind {
	case "max_duration", "task_not_found", "duplicate_task_id", "missing_config", "task_parse_failed":
		return *result.FailureKind, true
	default:
		return "", false
	}
}

func workerClaimResponse(row db.ClaimRunExecutionRow) api.WorkerClaim {
	return api.WorkerClaim{
		ID:        ids.MustFromPG(row.ExecutionID).String(),
		RunID:     ids.MustFromPG(row.ID).String(),
		WorkerID:  row.ExecutionWorkerID,
		ExpiresAt: pgTime(row.ExecutionLeaseExpiresAt),
	}
}

func (s *Server) workerRunFromClaim(ctx context.Context, worker workerActor, row db.ClaimRunExecutionRow) (api.WorkerRun, error) {
	restore, err := s.workerRestorePayload(ctx, row)
	if err != nil {
		return api.WorkerRun{}, err
	}
	var bindings api.SecretBindings
	if len(row.SecretBindings) > 0 {
		if err := json.Unmarshal(row.SecretBindings, &bindings); err != nil {
			return api.WorkerRun{}, terminalPayload("secret_unavailable", fmt.Errorf("decode secret bindings: %w", err))
		}
	}
	if bindings == nil {
		bindings = api.SecretBindings{}
	}
	if err := s.ensureWorkerWorkspaceSourceAuthorized(ctx, worker, row); err != nil {
		return api.WorkerRun{}, err
	}
	var resolvedSecrets api.ResolvedSecrets
	if len(bindings) > 0 && restore == nil {
		if s.secrets == nil {
			return api.WorkerRun{}, errors.New("secret store is not configured")
		}
		resolvedSecrets, err = s.secrets.ResolveScoped(ctx, ids.MustFromPG(row.OrgID), worker.ProjectID, worker.EnvironmentID, bindings)
		if err != nil {
			if secret.IsUnavailable(err) || errors.Is(err, pgx.ErrNoRows) {
				return api.WorkerRun{}, terminalPayload("secret_unavailable", err)
			}
			return api.WorkerRun{}, err
		}
	}
	run := api.WorkerRun{
		ID:      ids.MustFromPG(row.ID).String(),
		TaskID:  row.TaskID,
		Payload: json.RawMessage(row.Payload),
		Secrets: resolvedSecrets,
		TaskSource: api.TaskSourceArtifact{
			Digest:    row.TaskSourceDigest,
			MediaType: api.TaskSourceArtifactMediaType,
		},
		Workspace: api.GitHubSource{
			Repository: row.WorkspaceRepository,
			Ref:        row.WorkspaceRef,
			SHA:        row.WorkspaceSha,
			Subpath:    row.WorkspaceSubpath,
		},
		DeployedTask: api.WorkerDeployedTask{
			ID:         ids.MustFromPG(row.DeployedTaskID).String(),
			ModulePath: row.DeployedTaskModulePath,
			ExportName: row.DeployedTaskExportName,
		},
		MaxDurationSeconds: row.MaxDurationSeconds,
		ActiveDurationMs:   row.ActiveDurationMs,
		Restore:            restore,
	}
	if err := s.ensureWorkerWorkspaceSourceAuthorized(ctx, worker, row); err != nil {
		return api.WorkerRun{}, err
	}
	if restore == nil {
		workspaceToken, err := s.github.CreateRepositoryToken(ctx, row.WorkspaceInstallationID, row.WorkspaceGithubRepositoryID)
		if err != nil {
			if ghapp.IsInvalidSource(err) || ghapp.IsTerminalAccessError(err) {
				return api.WorkerRun{}, terminalPayload("workspace_unavailable", err)
			}
			return api.WorkerRun{}, err
		}
		run.WorkspaceCheckoutToken = &api.WorkerCheckoutToken{Token: workspaceToken.Token, ExpiresAt: workspaceToken.ExpiresAt}
	}
	return run, nil
}

func (s *Server) ensureWorkerWorkspaceSourceAuthorized(ctx context.Context, worker workerActor, row db.ClaimRunExecutionRow) error {
	source, err := s.db.GetActiveProjectWorkspaceRepositoryAccess(ctx, db.GetActiveProjectWorkspaceRepositoryAccessParams{
		OrgID:              row.OrgID,
		ProjectID:          ids.ToPG(worker.ProjectID),
		GithubRepositoryID: row.WorkspaceGithubRepositoryID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return terminalPayload("workspace_unavailable", fmt.Errorf("github repository %q is no longer enabled for this project workspace", row.WorkspaceRepository))
	}
	if err != nil {
		return err
	}
	if source.InstallationID != row.WorkspaceInstallationID || source.GithubRepositoryID != row.WorkspaceGithubRepositoryID {
		return terminalPayload("workspace_unavailable", fmt.Errorf("github repository %q connection changed after run creation", row.WorkspaceRepository))
	}
	return nil
}

func normalizeWorkerCapabilities(input api.WorkerCapabilities) (api.WorkerCapabilities, error) {
	capabilities := api.WorkerCapabilities{
		RuntimeArch:    strings.TrimSpace(input.RuntimeArch),
		RuntimeABI:     strings.TrimSpace(input.RuntimeABI),
		KernelDigest:   strings.TrimSpace(input.KernelDigest),
		RootfsDigest:   strings.TrimSpace(input.RootfsDigest),
		CNIProfile:     strings.TrimSpace(input.CNIProfile),
		MaxVCPUs:       input.MaxVCPUs,
		MaxMemoryMiB:   input.MaxMemoryMiB,
		SlotsAvailable: input.SlotsAvailable,
	}
	if capabilities.RuntimeArch == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_arch is required")
	}
	if capabilities.RuntimeABI == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_abi is required")
	}
	if capabilities.KernelDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker kernel_digest is required")
	}
	if capabilities.RootfsDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker rootfs_digest is required")
	}
	if capabilities.CNIProfile == "" {
		return api.WorkerCapabilities{}, errors.New("worker cni_profile is required")
	}
	if capabilities.MaxVCPUs <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_vcpus must be positive")
	}
	if capabilities.MaxVCPUs > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_vcpus exceeds max %d", math.MaxInt32)
	}
	if capabilities.MaxMemoryMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_memory_mib must be positive")
	}
	if capabilities.MaxMemoryMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_memory_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.SlotsAvailable <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker slots_available must be positive")
	}
	return capabilities, nil
}

func (s *Server) workerRestorePayload(ctx context.Context, row db.ClaimRunExecutionRow) (*api.WorkerRestore, error) {
	payload, err := s.db.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:        row.OrgID,
		RunID:        row.ID,
		ExecutionID:  row.ExecutionID,
		WorkerPoolID: row.ExecutionWorkerPoolID,
		WorkerID:     row.ExecutionWorkerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var memoryDigests []string
	if len(payload.MemoryDigests) > 0 {
		if err := json.Unmarshal(payload.MemoryDigests, &memoryDigests); err != nil {
			return nil, fmt.Errorf("decode checkpoint memory digests: %w", err)
		}
	}
	return &api.WorkerRestore{
		CheckpointID: ids.MustFromPG(payload.CheckpointID).String(),
		Checkpoint: api.WorkerCheckpointManifest{
			RuntimeBackend:       payload.RuntimeBackend.String,
			RuntimeArch:          payload.RuntimeArch.String,
			RuntimeABI:           payload.RuntimeABI.String,
			KernelDigest:         pgTextStringPtr(payload.KernelDigest),
			RootfsDigest:         pgTextStringPtr(payload.RootfsDigest),
			ImageKey:             pgTextStringPtr(payload.ImageKey),
			RuntimeConfigDigest:  pgTextStringPtr(payload.RuntimeConfigDigest),
			ManifestDigest:       pgTextStringPtr(payload.ManifestDigest),
			VMStateDigest:        pgTextStringPtr(payload.VMStateDigest),
			WorkspaceUpperDigest: pgTextStringPtr(payload.WorkspaceUpperDigest),
			MemoryDigests:        memoryDigests,
			Manifest:             json.RawMessage(payload.Manifest),
		},
		Waitpoint: api.WorkerRestoreWaitpoint{
			ID:                    ids.MustFromPG(payload.WaitpointID).String(),
			Kind:                  string(payload.WaitpointKind),
			ResolutionKind:        payload.ResolutionKind.String,
			ResolutionPayloadJSON: json.RawMessage(payload.Resolution),
		},
	}, nil
}

func pgTextStringPtr(value pgtype.Text) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}
