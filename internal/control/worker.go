package control

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

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/dispatcher"
	"github.com/helmrdotdev/helmr/internal/ghapp"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	"github.com/helmrdotdev/helmr/internal/runqueue/claimer"
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
		writeError(w, http.StatusServiceUnavailable, errors.New("worker bootstrap storage is not configured"))
		return
	}
	if len(s.authSecret) == 0 {
		writeError(w, http.StatusServiceUnavailable, errors.New("worker bootstrap is not configured"))
		return
	}
	var request api.WorkerRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker bootstrap request JSON: %w", err))
		return
	}
	registrationHash, err := auth.HashToken(s.authSecret, request.BootstrapToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker bootstrap token is required"))
		return
	}
	if strings.TrimSpace(request.BootstrapToken) == s.workerRegisterToken {
		if err := s.ensureWorkerBootstrapToken(r.Context(), s.db); err != nil {
			s.log.Error("worker bootstrap token bootstrap failed", "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("configure worker bootstrap token"))
			return
		}
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authSecret)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := ids.New()
	resourceID := strings.TrimSpace(request.ResourceID)
	if resourceID == "" {
		resourceID = workerInstanceID.String()
	}
	credential, err := s.db.CreateWorkerInstanceCredentialFromBootstrap(r.Context(), db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: registrationHash,
		CredentialID:       ids.ToPG(ids.New()),
		WorkerInstanceID:   ids.ToPG(workerInstanceID),
		ResourceID:         resourceID,
		KeyPrefix:          generated.KeyPrefix,
		SecretHash:         generated.TokenHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker bootstrap token is invalid"))
		return
	}
	if err != nil {
		s.log.Error("worker bootstrap failed", "worker_instance_id", workerInstanceID.String(), "resource_id", resourceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("register worker"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerRegisterResponse{
		WorkerInstanceID:     ids.MustFromPG(credential.WorkerInstanceID).String(),
		WorkerInstanceSecret: generated.Raw,
	})
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
	request.WorkerInstanceID = strings.TrimSpace(request.WorkerInstanceID)
	if request.WorkerInstanceID == "" {
		writeError(w, http.StatusBadRequest, errors.New("worker_instance_id is required"))
		return
	}
	workerInstanceID, err := ids.Parse(request.WorkerInstanceID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("worker_instance_id must be a UUID"))
		return
	}
	secretHash, err := auth.HashToken(s.authSecret, request.WorkerInstanceSecret)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	credential, err := s.db.AuthenticateWorkerInstanceCredential(r.Context(), db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: ids.ToPG(workerInstanceID),
		SecretHash:       secretHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, errors.New("worker authentication is required"))
		return
	}
	if err != nil {
		s.log.Error("worker instance credential authentication failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("worker authentication"))
		return
	}
	credentialID, err := ids.FromPG(credential.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("worker instance credential id"))
		return
	}
	now := time.Now()
	expiresAt := now.Add(s.workerTokenTTL)
	signed, err := auth.IssueWorkerToken(s.workerTokenSecret, auth.WorkerClaims{
		WorkerInstanceID: ids.MustFromPG(credential.WorkerInstanceID).String(),
		CredentialID:     credentialID.String(),
		IssuedAt:         now,
		ExpiresAt:        expiresAt,
	})
	if err != nil {
		s.log.Error("mint worker token failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerTokenResponse{
		Token:            signed,
		ExpiresInSeconds: int64(s.workerTokenTTL / time.Second),
	})
}

func (s *Server) workerLease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.runQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	if s.github == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("github resolver is not configured"))
		return
	}
	var request api.WorkerRunLeaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker run lease request JSON: %w", err))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("record worker heartbeat"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), ids.ToPG(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}
	runClaimer, err := claimer.New(s.db, s.runQueue)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	dequeueRequest := runqueue.DequeueRequest{
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		Available: compute.ResourceVector{
			MilliCPU:  capacity.AvailableMilliCpu,
			MemoryMiB: capacity.AvailableMemoryMib,
			DiskMiB:   capacity.AvailableDiskMib,
			Slots:     capacity.AvailableExecutionSlots,
		},
		Runtime: compute.RuntimeSelector{
			Arch:         capabilities.RuntimeArch,
			ABI:          capabilities.RuntimeABI,
			KernelDigest: capabilities.KernelDigest,
			RootfsDigest: capabilities.RootfsDigest,
			CNIProfile:   capabilities.CNIProfile,
		},
		Region:      capabilities.Region,
		Labels:      capabilities.Labels,
		MaxMessages: 1,
	}
	var queueLease claimer.Result
	var leasedRun db.LeaseRunExecutionRow
	const scopePageSize int32 = 100
	scanSeed := int32(s.workerLeaseScanSeed.Add(1) & 0x7fffffff)
	foundLease := false
	for rowOffset := int32(0); !foundLease; rowOffset += scopePageSize {
		scopes, err := s.db.ListQueueScopes(r.Context(), db.ListQueueScopesParams{
			ScanSeed:  fmt.Sprint(scanSeed),
			RowOffset: rowOffset,
			RowLimit:  scopePageSize,
		})
		if err != nil {
			s.log.Error("worker queue scope lookup failed", "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("list worker queue scopes"))
			return
		}
		if len(scopes) == 0 {
			break
		}
		for _, scope := range scopes {
			orgID := ids.MustFromPG(scope.OrgID)
			if err := dispatcher.SweepOnceForOrg(r.Context(), s.db, scope.OrgID); err != nil {
				s.log.Warn("sweep expired executions failed", "org_id", orgID.String(), "error", err)
			}
			dequeueRequest.OrgID = orgID.String()
			for _, queueName := range runqueue.QueueNamesForRuntime(scope.QueueName, dequeueRequest.Runtime) {
				dequeueRequest.QueueName = queueName
				candidateLease, err := runClaimer.Lease(r.Context(), claimer.LeaseRequest{DequeueRequest: dequeueRequest})
				if errors.Is(err, claimer.ErrNoLease) {
					continue
				}
				if err != nil {
					s.log.Error("worker queue lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
					writeError(w, http.StatusInternalServerError, errors.New("lease run queue item"))
					return
				}
				if candidateLease.Lease.MessageID == "" {
					continue
				}
				candidateRun, err := s.db.LeaseRunExecution(r.Context(), db.LeaseRunExecutionParams{
					OrgID:             candidateLease.Entry.OrgID,
					RunID:             candidateLease.Entry.RunID,
					WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
					ExecutionID:       ids.ToPG(ids.New()),
					DispatchMessageID: pgtype.Text{String: candidateLease.Lease.MessageID, Valid: true},
					DispatchLeaseID:   candidateLease.Lease.ID,
					DispatchAttempt:   candidateLease.Lease.AttemptNumber,
					LeaseExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(workerLeaseDuration), Valid: true},
				})
				if err == nil {
					queueLease = candidateLease
					leasedRun = candidateRun
					foundLease = true
					break
				}
				if errors.Is(err, pgx.ErrNoRows) {
					s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, runqueue.NackReasonLeaseConflict, "execution lease conflict")
					continue
				}
				s.requeueWorkerQueueItem(r.Context(), worker, candidateLease.Entry.RunID, candidateLease.Lease, runqueue.NackReasonRetry, err.Error())
				s.log.Error("worker run lease failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
				writeError(w, http.StatusInternalServerError, errors.New("lease run"))
				return
			}
			if foundLease {
				break
			}
		}
		if int32(len(scopes)) < scopePageSize {
			break
		}
	}
	if !foundLease {
		writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
		return
	}

	lease := workerRunLeaseResponse(leasedRun)
	run, err := s.workerRunFromLease(r.Context(), leasedRun)
	if err != nil {
		if failure, ok := terminalPayloadFailure(err); ok {
			if failErr := s.failLeasedRunPayload(r.Context(), leasedRun, queueLease.Lease, failure); failErr != nil {
				s.log.Error("fail worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", failErr)
				writeError(w, http.StatusInternalServerError, errors.New("fail worker run payload"))
				return
			}
			s.log.Warn("terminal worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "failure_kind", failure.kind, "error", err)
			writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{})
			return
		}
		if abandonErr := s.db.AbandonLeasedRunExecution(r.Context(), db.AbandonLeasedRunExecutionParams{
			OrgID:            leasedRun.OrgID,
			RunID:            leasedRun.ID,
			ExecutionID:      leasedRun.ExecutionID,
			WorkerInstanceID: leasedRun.ExecutionWorkerInstanceID,
		}); abandonErr != nil {
			s.log.Error("abandon worker run lease failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", abandonErr)
		}
		s.requeueWorkerQueueItem(r.Context(), worker, leasedRun.ID, queueLease.Lease, runqueue.NackReasonRetry, err.Error())
		s.log.Error("build worker run payload failed", "run_id", ids.MustFromPG(leasedRun.ID).String(), "execution_id", ids.MustFromPG(leasedRun.ExecutionID).String(), "error", err)
		writeError(w, http.StatusBadGateway, errors.New("build worker run payload"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerRunLeaseResponse{Lease: &lease, Run: &run})
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
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker activate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("activate worker"))
		return
	}
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     ids.ToPG(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusActive,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	} else if err != nil {
		s.log.Error("worker activate status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
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
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     ids.ToPG(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusDraining,
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	} else if err != nil {
		s.log.Error("worker drain failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
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
	state, err := s.db.GetWorkerInstanceState(r.Context(), ids.ToPG(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("worker is not registered"))
		return
	}
	if err != nil {
		s.log.Error("get worker status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get worker status"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStatusResponse{
		WorkerInstanceID: ids.MustFromPG(state.ID).String(),
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
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	status, err := s.db.StartRunExecution(r.Context(), db.StartRunExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker start failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("start run"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStartResponse{RunID: request.Lease.RunID, Status: string(status)})
}

func (s *Server) workerRenew(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.runQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerRenewRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker renew request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	leaseRow, queueLease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
			return
		}
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return
	}
	expiresAt := time.Now().Add(workerLeaseDuration)
	if _, err := s.db.RenewRunQueueReservation(r.Context(), db.RenewRunQueueReservationParams{
		OrgID:                ids.ToPG(leaseIDs.orgID),
		RunID:                ids.ToPG(leaseIDs.runID),
		WorkerInstanceID:     ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID:    pgtype.Text{String: leaseRow.DispatchMessageID, Valid: true},
		ReservationExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	} else if err != nil {
		s.log.Error("worker queue lease renewal failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("renew queue lease"))
		return
	}
	renewed, err := s.db.RenewRunExecutionLease(r.Context(), db.RenewRunExecutionLeaseParams{
		OrgID:             ids.ToPG(leaseIDs.orgID),
		RunID:             ids.ToPG(leaseIDs.runID),
		ExecutionID:       ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID: leaseRow.DispatchMessageID,
		DispatchLeaseID:   leaseRow.DispatchLeaseID,
		LeaseExpiresAt:    pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker renew failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("renew run execution"))
		return
	}
	if _, err := s.runQueue.Renew(r.Context(), queueLease, expiresAt); err != nil {
		s.log.Warn("worker dispatch renew repair failed", "run_id", request.Lease.RunID, "error", err)
	}
	lease := api.WorkerRunLease{
		ID:                request.Lease.ID,
		OrgID:             request.Lease.OrgID,
		RunID:             request.Lease.RunID,
		WorkerInstanceID:  ids.MustFromPG(renewed.WorkerInstanceID).String(),
		DispatchMessageID: renewed.DispatchMessageID,
		DispatchLeaseID:   renewed.DispatchLeaseID,
		ExpiresAt:         pgTime(renewed.LeaseExpiresAt),
	}
	writeJSON(w, http.StatusOK, api.WorkerRenewResponse{Lease: lease})
}

func (s *Server) workerRelease(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if s.runQueue == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run queue item queue is not configured"))
		return
	}
	var request api.WorkerReleaseRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid worker release request JSON: %w", err))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return
	}
	_, lease, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	activeQueueLeaseFound := err == nil
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
			writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
			return
		}
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
		OrgID:                ids.ToPG(leaseIDs.orgID),
		RunID:                ids.ToPG(leaseIDs.runID),
		ExecutionID:          ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID:     ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID:    leaseIDs.queueMessageID,
		DispatchLeaseID:      leaseIDs.queueLeaseID,
		Status:               status,
		ExitCode:             exitCode,
		Output:               output,
		ErrorMessage:         errorMessage,
		TerminalEventKind:    terminalEventKind,
		TerminalEventPayload: terminalEventPayload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("worker release failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("release run"))
		return
	}
	if activeQueueLeaseFound {
		s.ackWorkerQueueLease(r.Context(), ids.ToPG(leaseIDs.runID), lease)
	}
	writeJSON(w, http.StatusOK, api.WorkerReleaseResponse{RunID: request.Lease.RunID, Status: string(run.Status)})
}

func (s *Server) ackWorkerQueueLease(ctx context.Context, runID pgtype.UUID, lease runqueue.Lease) {
	if err := s.runQueue.Ack(ctx, lease); err != nil {
		s.log.Warn("complete queue lease failed", "run_id", ids.MustFromPG(runID).String(), "error", err)
	}
}

func (s *Server) requeueWorkerQueueItem(ctx context.Context, worker workerActor, runID pgtype.UUID, lease runqueue.Lease, reason runqueue.NackReason, lastError string) {
	orgID, err := ids.Parse(lease.Message.OrgID)
	if err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
		if nackErr := s.runQueue.Nack(ctx, lease, runqueue.NackReasonInvalid); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", runqueue.NackReasonInvalid, "error", nackErr)
		}
		return
	}
	if _, err := s.db.RequeueRunQueueItem(ctx, db.RequeueRunQueueItemParams{
		OrgID:             ids.ToPG(orgID),
		RunID:             runID,
		WorkerInstanceID:  ids.ToPG(worker.WorkerInstanceID),
		DispatchMessageID: pgtype.Text{String: lease.MessageID, Valid: true},
		LastError:         strings.TrimSpace(lastError),
	}); err != nil {
		s.log.Warn("requeue run queue item failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
		nackReason := reason
		if errors.Is(err, pgx.ErrNoRows) {
			nackReason = runqueue.NackReasonInvalid
		}
		if nackErr := s.runQueue.Nack(ctx, lease, nackReason); nackErr != nil {
			s.log.Warn("requeue queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", nackReason, "error", nackErr)
		}
		return
	}
	if err := s.runQueue.Nack(ctx, lease, runqueue.NackReasonInvalid); err != nil {
		s.log.Warn("discard stale queue lease failed", "run_id", ids.MustFromPG(runID).String(), "reason", reason, "error", err)
	}
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
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"run_id":       request.Lease.RunID,
		"stream":       request.Stream,
		"observed_seq": request.ObservedSeq,
		"bytes":        len(content),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode worker log event"))
		return
	}
	_, err = s.db.AppendRunLogChunk(r.Context(), db.AppendRunLogChunkParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		Stream:           db.RunLogStream(request.Stream),
		ObservedSeq:      int64(request.ObservedSeq),
		Content:          content,
		Kind:             kind,
		Payload:          payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker logs failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker logs"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
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
	s.appendWorkerEvent(w, r, request.Lease, "log", payload)
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
	s.appendWorkerEvent(w, r, request.Lease, "emit."+request.EventType, payload)
}

func (s *Server) appendWorkerEvent(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease, kind string, payload []byte) {
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, lease)
	if !ok {
		return
	}
	_, err := s.db.AppendRunEventForExecution(r.Context(), db.AppendRunEventForExecutionParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
		Kind:             kind,
		Payload:          payload,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return
	}
	if err != nil {
		s.log.Error("append worker event failed", "run_id", lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("append worker event"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: lease.RunID})
}

func (s *Server) workerRunLeaseForWrite(w http.ResponseWriter, r *http.Request, lease api.WorkerRunLease) (workerActor, workerRunLeaseIDs, bool) {
	leaseIDs, err := parseWorkerRunLease(lease)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	worker := workerFromContext(r.Context())
	if lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, http.StatusForbidden, errors.New("worker run lease belongs to another worker"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	if _, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs); errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("worker run lease is stale"))
		return workerActor{}, workerRunLeaseIDs{}, false
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", lease.RunID, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("get queue lease"))
		return workerActor{}, workerRunLeaseIDs{}, false
	}
	return worker, leaseIDs, true
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

func (s *Server) failLeasedRunPayload(ctx context.Context, row db.LeaseRunExecutionRow, lease runqueue.Lease, failure payloadFailure) error {
	kind, payload, err := payloadFailureRunEvent(failure)
	if err != nil {
		return err
	}
	_, err = s.db.ReleaseRunExecution(ctx, db.ReleaseRunExecutionParams{
		OrgID:                row.OrgID,
		RunID:                row.ID,
		ExecutionID:          row.ExecutionID,
		WorkerInstanceID:     row.ExecutionWorkerInstanceID,
		DispatchMessageID:    row.ExecutionDispatchMessageID,
		DispatchLeaseID:      row.ExecutionDispatchLeaseID,
		Status:               db.RunStatusFailed,
		ExitCode:             pgtype.Int4{},
		ErrorMessage:         pgtype.Text{String: failure.message, Valid: true},
		TerminalEventKind:    kind,
		TerminalEventPayload: payload,
	})
	if err != nil {
		return err
	}
	s.ackWorkerQueueLease(ctx, row.ID, lease)
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

type workerRunLeaseIDs struct {
	orgID          uuid.UUID
	executionID    uuid.UUID
	runID          uuid.UUID
	queueMessageID string
	queueLeaseID   string
}

func parseWorkerRunLease(lease api.WorkerRunLease) (workerRunLeaseIDs, error) {
	if strings.TrimSpace(lease.WorkerInstanceID) == "" {
		return workerRunLeaseIDs{}, errors.New("lease.worker_instance_id is required")
	}
	orgID, err := ids.Parse(lease.OrgID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.org_id must be a UUID")
	}
	executionID, err := ids.Parse(lease.ID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.id must be a UUID")
	}
	runID, err := ids.Parse(lease.RunID)
	if err != nil {
		return workerRunLeaseIDs{}, errors.New("lease.run_id must be a UUID")
	}
	queueMessageID := strings.TrimSpace(lease.DispatchMessageID)
	if queueMessageID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_message_id is required")
	}
	queueLeaseID := strings.TrimSpace(lease.DispatchLeaseID)
	if queueLeaseID == "" {
		return workerRunLeaseIDs{}, errors.New("lease.dispatch_lease_id is required")
	}
	return workerRunLeaseIDs{
		orgID:          orgID,
		executionID:    executionID,
		runID:          runID,
		queueMessageID: queueMessageID,
		queueLeaseID:   queueLeaseID,
	}, nil
}

func (s *Server) workerExecutionLease(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs) (db.GetRunExecutionQueueLeaseRow, runqueue.Lease, error) {
	row, err := s.db.GetRunExecutionQueueLease(ctx, db.GetRunExecutionQueueLeaseParams{
		OrgID:            ids.ToPG(leaseIDs.orgID),
		RunID:            ids.ToPG(leaseIDs.runID),
		ExecutionID:      ids.ToPG(leaseIDs.executionID),
		WorkerInstanceID: ids.ToPG(worker.WorkerInstanceID),
	})
	if err != nil {
		return db.GetRunExecutionQueueLeaseRow{}, runqueue.Lease{}, err
	}
	if row.DispatchMessageID != leaseIDs.queueMessageID || row.DispatchLeaseID != leaseIDs.queueLeaseID {
		return db.GetRunExecutionQueueLeaseRow{}, runqueue.Lease{}, pgx.ErrNoRows
	}
	lease := runqueue.Lease{
		ID:               row.DispatchLeaseID,
		MessageID:        row.DispatchMessageID,
		WorkerInstanceID: worker.WorkerInstanceID.String(),
		AttemptNumber:    row.DispatchAttempt,
		ExpiresAt:        pgTime(row.LeaseExpiresAt),
		Message: runqueue.Message{
			OrgID:     leaseIDs.orgID.String(),
			RunID:     ids.MustFromPG(row.RunID).String(),
			QueueName: row.QueueName,
		},
	}
	return row, lease, nil
}

func workerInstanceHeartbeatParams(worker workerActor, capabilities api.WorkerCapabilities) db.UpsertWorkerInstanceHeartbeatParams {
	resources := compute.ResourceVector{
		MilliCPU:  capabilities.MaxVCPUs * 1000,
		MemoryMiB: capabilities.MaxMemoryMiB,
		DiskMiB:   capabilities.MaxDiskMiB,
		Slots:     capabilities.ExecutionSlotsAvailable,
	}
	heartbeat, _ := json.Marshal(map[string]any{
		"runtime_arch":  capabilities.RuntimeArch,
		"runtime_abi":   capabilities.RuntimeABI,
		"kernel_digest": capabilities.KernelDigest,
		"rootfs_digest": capabilities.RootfsDigest,
		"cni_profile":   capabilities.CNIProfile,
	})
	labels, _ := json.Marshal(capabilities.Labels)
	return db.UpsertWorkerInstanceHeartbeatParams{
		ID:                      ids.ToPG(worker.WorkerInstanceID),
		ResourceID:              worker.ResourceID,
		Region:                  capabilities.Region,
		TotalMilliCpu:           resources.MilliCPU,
		TotalMemoryMib:          resources.MemoryMiB,
		TotalDiskMib:            resources.DiskMiB,
		TotalExecutionSlots:     resources.Slots,
		AvailableMilliCpu:       resources.MilliCPU,
		AvailableMemoryMib:      resources.MemoryMiB,
		AvailableDiskMib:        resources.DiskMiB,
		AvailableExecutionSlots: resources.Slots,
		Labels:                  labels,
		Heartbeat:               heartbeat,
	}
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

func workerRunLeaseResponse(row db.LeaseRunExecutionRow) api.WorkerRunLease {
	return api.WorkerRunLease{
		ID:                ids.MustFromPG(row.ExecutionID).String(),
		OrgID:             ids.MustFromPG(row.OrgID).String(),
		RunID:             ids.MustFromPG(row.ID).String(),
		WorkerInstanceID:  ids.MustFromPG(row.ExecutionWorkerInstanceID).String(),
		DispatchMessageID: row.ExecutionDispatchMessageID,
		DispatchLeaseID:   row.ExecutionDispatchLeaseID,
		ExpiresAt:         pgTime(row.ExecutionLeaseExpiresAt),
	}
}

func (s *Server) workerRunFromLease(ctx context.Context, row db.LeaseRunExecutionRow) (api.WorkerRun, error) {
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
	var resolvedSecrets api.ResolvedSecrets
	if len(bindings) > 0 && restore == nil {
		if s.secrets == nil {
			return api.WorkerRun{}, errors.New("secret store is not configured")
		}
		resolvedSecrets, err = s.secrets.ResolveScoped(ctx, ids.MustFromPG(row.OrgID), ids.MustFromPG(row.ProjectID), ids.MustFromPG(row.EnvironmentID), bindings)
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
		DeploymentTask: api.WorkerDeploymentTask{
			ID:         ids.MustFromPG(row.DeploymentTaskID).String(),
			ModulePath: row.DeploymentTaskModulePath,
			ExportName: row.DeploymentTaskExportName,
		},
		MaxDurationSeconds: row.MaxDurationSeconds,
		ActiveDurationMs:   row.ActiveDurationMs,
		Restore:            restore,
	}
	if err := s.ensureWorkerWorkspaceSourceAuthorized(ctx, row); err != nil {
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

func (s *Server) ensureWorkerWorkspaceSourceAuthorized(ctx context.Context, row db.LeaseRunExecutionRow) error {
	source, err := s.db.GetActiveProjectGitHubRepository(ctx, db.GetActiveProjectGitHubRepositoryParams{
		OrgID:              row.OrgID,
		ProjectID:          row.ProjectID,
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
		RuntimeArch:             strings.TrimSpace(input.RuntimeArch),
		RuntimeABI:              strings.TrimSpace(input.RuntimeABI),
		KernelDigest:            strings.TrimSpace(input.KernelDigest),
		RootfsDigest:            strings.TrimSpace(input.RootfsDigest),
		CNIProfile:              strings.TrimSpace(input.CNIProfile),
		Region:                  strings.TrimSpace(input.Region),
		MaxVCPUs:                input.MaxVCPUs,
		MaxMemoryMiB:            input.MaxMemoryMiB,
		MaxDiskMiB:              input.MaxDiskMiB,
		ExecutionSlotsAvailable: input.ExecutionSlotsAvailable,
	}
	labels, err := normalizeWorkerLabels(input.Labels)
	if err != nil {
		return api.WorkerCapabilities{}, err
	}
	capabilities.Labels = labels
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
	if capabilities.MaxDiskMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_disk_mib must be positive")
	}
	if capabilities.MaxDiskMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_disk_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.ExecutionSlotsAvailable <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker execution_slots_available must be positive")
	}
	return capabilities, nil
}

func normalizeWorkerLabels(input map[string]string) (map[string]string, error) {
	labels := make(map[string]string, len(input))
	for key, value := range input {
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, errors.New("worker label key is required")
		}
		labels[key] = value
	}
	return labels, nil
}

func (s *Server) workerRestorePayload(ctx context.Context, row db.LeaseRunExecutionRow) (*api.WorkerRestore, error) {
	payload, err := s.db.GetRunRestorePayload(ctx, db.GetRunRestorePayloadParams{
		OrgID:            row.OrgID,
		RunID:            row.ID,
		ExecutionID:      row.ExecutionID,
		WorkerInstanceID: row.ExecutionWorkerInstanceID,
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
