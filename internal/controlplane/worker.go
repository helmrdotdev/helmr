package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/deployment"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultWorkerTokenTTL = 15 * time.Minute

const workerEnrollmentNonceTTL = 2 * time.Minute

func (s *Server) workerEnrollmentChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.workerEnrollmentGuard.allowChallenge(workerEnrollmentSource(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, tooManyRequests(errors.New("worker enrollment challenge rate limit exceeded")))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	var request workerapi.EnrollmentChallengeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker enrollment challenge JSON: %w", err)))
		return
	}
	request.WorkerGroupID = strings.TrimSpace(request.WorkerGroupID)
	if request.WorkerGroupID == "" {
		writeError(w, badRequest(errors.New("worker_group_id is required")))
		return
	}
	if !s.workerEnrollment.HasGroup(request.WorkerGroupID) {
		writeError(w, notFound(errors.New("worker group not found")))
		return
	}
	rawNonce := make([]byte, 32)
	if _, err := rand.Read(rawNonce); err != nil {
		writeError(w, errors.New("generate worker enrollment challenge"))
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(rawNonce)
	nonceHash, err := auth.HashToken(s.authKeys.WorkerEnrollment, nonce)
	if err != nil {
		writeError(w, unavailable(errors.New("worker enrollment is not configured")))
		return
	}
	expiresAt := time.Now().UTC().Add(workerEnrollmentNonceTTL)
	created, err := s.db.CreateWorkerEnrollmentNonce(r.Context(), db.CreateWorkerEnrollmentNonceParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		NonceHash:     nonceHash,
		WorkerGroupID: request.WorkerGroupID,
		ExpiresAt:     pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker group not found")))
		return
	}
	if err != nil {
		s.log.Error("create worker enrollment challenge failed", "worker_group_id", request.WorkerGroupID, "error", err)
		writeError(w, errors.New("create worker enrollment challenge"))
		return
	}
	writeJSON(w, http.StatusCreated, workerapi.EnrollmentChallengeResponse{
		Nonce:           nonce,
		WorkerGroupID:   created.WorkerGroupID,
		ExpiresAt:       pgvalue.Time(created.ExpiresAt),
		ProtocolVersion: workerapi.CurrentProtocolVersion,
	})
}

func (s *Server) workerEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.workerEnrollmentGuard.allowEnrollment(workerEnrollmentSource(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, tooManyRequests(errors.New("worker enrollment rate limit exceeded")))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request workerapi.EnrollmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker enrollment JSON: %w", err)))
		return
	}
	if request.WorkerGroupID == "" || request.ProtocolVersion != workerapi.CurrentProtocolVersion || (!request.SupportsRun && !request.SupportsBuild) {
		writeError(w, badRequest(errors.New("worker_group_id, protocol_version, and at least one supported role are required")))
		return
	}
	if request.Nonce == "" {
		writeError(w, badRequest(errors.New("nonce is required")))
		return
	}
	nonceHash, err := auth.HashToken(s.authKeys.WorkerEnrollment, request.Nonce)
	if err != nil {
		writeError(w, unauthorized(errors.New("worker enrollment challenge is invalid")))
		return
	}
	if _, err := s.db.GetActiveWorkerEnrollmentNonce(r.Context(), db.GetActiveWorkerEnrollmentNonceParams{
		NonceHash: nonceHash, WorkerGroupID: request.WorkerGroupID,
	}); isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker enrollment challenge is invalid or expired")))
		return
	} else if err != nil {
		s.log.Error("worker enrollment challenge lookup failed", "worker_group_id", request.WorkerGroupID, "error", err)
		writeError(w, errors.New("verify worker enrollment challenge"))
		return
	}
	if !s.workerEnrollmentGuard.beginVerification(r.Context()) {
		writeError(w, unavailable(errors.New("worker enrollment verification was canceled")))
		return
	}
	defer s.workerEnrollmentGuard.endVerification()
	if err := s.workerEnrollment.Verify(request); err != nil {
		s.log.Warn("worker enrollment evidence rejected", "worker_group_id", request.WorkerGroupID)
		writeError(w, unauthorized(errors.New("worker enrollment evidence is invalid")))
		return
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authKeys.WorkerInstance)
	if err != nil {
		writeError(w, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := uuid.Must(uuid.NewV7())
	credential, err := s.db.EnrollWorkerInstance(r.Context(), db.EnrollWorkerInstanceParams{
		NonceHash:        nonceHash,
		WorkerGroupID:    request.WorkerGroupID,
		AllowsRun:        request.SupportsRun,
		AllowsBuild:      request.SupportsBuild,
		ProtocolVersion:  request.ProtocolVersion,
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		CurrentServiceID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ResourceID:       request.ResourceID,
		CredentialID:     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		KeyPrefix:        generated.KeyPrefix,
		SecretHash:       generated.TokenHash,
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker enrollment challenge is invalid or expired")))
		return
	}
	if err != nil {
		s.log.Error("worker enrollment failed", "worker_group_id", request.WorkerGroupID, "resource_id", request.ResourceID, "error", err)
		writeError(w, errors.New("enroll worker"))
		return
	}
	writeJSON(w, http.StatusCreated, workerapi.EnrollmentResponse{
		WorkerInstanceID:     pgvalue.MustUUIDValue(credential.WorkerInstanceID).String(),
		WorkerGroupID:        credential.WorkerGroupID,
		WorkerInstanceSecret: generated.Raw,
	})
}

func (s *Server) workerAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || !s.authKeys.Valid() || len(s.workerTokenSigningKey) == 0 {
		writeError(w, unavailable(errors.New("worker authentication is not configured")))
		return
	}
	var request workerapi.TokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker token request JSON: %w", err)))
		return
	}
	if request.WorkerInstanceID == "" {
		writeError(w, badRequest(errors.New("worker_instance_id is required")))
		return
	}
	workerInstanceID, err := ids.Parse(request.WorkerInstanceID)
	if err != nil {
		writeError(w, badRequest(errors.New("worker_instance_id must be a canonical UUIDv7")))
		return
	}
	secretHash, err := auth.HashToken(s.authKeys.WorkerInstance, request.WorkerInstanceSecret)
	if err != nil {
		writeError(w, unauthorized(errors.New("worker authentication is required")))
		return
	}
	serviceID, err := ids.Parse(request.ServiceID)
	if err != nil {
		writeError(w, badRequest(errors.New("service_id must be a canonical UUIDv7")))
		return
	}
	protocolVersion := strings.TrimSpace(request.ProtocolVersion)
	if protocolVersion != workerapi.CurrentProtocolVersion {
		writeError(w, badRequest(fmt.Errorf("protocol_version must be %s", workerapi.CurrentProtocolVersion)))
		return
	}
	if !request.SupportsRun && !request.SupportsBuild {
		writeError(w, badRequest(errors.New("at least one supported worker role is required")))
		return
	}
	credential, err := s.db.AuthenticateWorkerInstanceCredential(r.Context(), db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		SecretHash:       secretHash,
		ServiceID:        pgvalue.UUID(serviceID),
		ProtocolVersion:  protocolVersion,
		SupportsRun:      request.SupportsRun,
		SupportsBuild:    request.SupportsBuild,
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker authentication is required")))
		return
	}
	if err != nil {
		s.log.Error("worker instance credential authentication failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, errors.New("worker authentication"))
		return
	}
	credentialID, err := pgvalue.UUIDValue(credential.ID)
	if err != nil {
		writeError(w, errors.New("worker instance credential id"))
		return
	}
	now := time.Now()
	expiresAt := now.Add(s.workerTokenTTL)
	if !credential.CurrentEpoch.Valid || credential.CurrentEpoch.Int64 <= 0 {
		writeError(w, errors.New("worker epoch was not established"))
		return
	}
	claims, err := (auth.WorkerTokenAuthority{
		WorkerInstanceID:  pgvalue.MustUUIDValue(credential.WorkerInstanceID),
		CredentialID:      credentialID,
		WorkerGroupID:     credential.WorkerGroupID,
		ClaimVersion:      credential.ClaimVersion,
		GroupClaimVersion: credential.GroupClaimVersion,
		WorkerEpoch:       credential.CurrentEpoch.Int64,
		CredentialRoles: auth.WorkerRoles{
			Run: credential.CredentialAllowsRun, Build: credential.CredentialAllowsBuild,
		},
		GroupRoles: auth.WorkerRoles{Run: credential.GroupAllowsRun, Build: credential.GroupAllowsBuild},
	}).Claims(auth.EpochExchangeInput{
		ServiceID: serviceID, SupervisorRoles: auth.WorkerRoles{Run: request.SupportsRun, Build: request.SupportsBuild},
		ProtocolVersion: protocolVersion,
	}, now, expiresAt)
	if errors.Is(err, auth.ErrWorkerRoleIntersectionEmpty) {
		writeError(w, forbidden(errors.New("worker has no allowed roles")))
		return
	}
	if err != nil {
		s.log.Error("derive worker token claims failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, errors.New("mint worker token"))
		return
	}
	signed, err := auth.IssueWorkerToken(s.workerTokenSigningKey, claims)
	if err != nil {
		s.log.Error("mint worker token failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.TokenResponse{
		Token:            signed,
		ExpiresInSeconds: int64(s.workerTokenTTL / time.Second),
		WorkerEpoch:      credential.CurrentEpoch.Int64,
		Roles:            claims.Roles,
		ProtocolVersion:  credential.ProtocolVersion,
	})
}

func (s *Server) workerActivate(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.ActivateRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker activate request JSON: %w", err)))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.validateWorkerBuildPolicy(capabilities); err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	_, err = s.db.ActivateWorkerInstance(
		r.Context(), workerActivationParams(worker, capabilities),
	)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker activation is stale")))
		return
	} else if err != nil {
		s.log.Error("worker activate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("activate worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerStartupRecovery(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker recovery storage is not configured")))
		return
	}
	var request workerapi.StartupRecoveryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker startup recovery JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if err := validateWorkerStartupRecovery(request, worker.EpochStartedAt, time.Now()); err != nil {
		writeError(w, badRequest(err))
		return
	}
	evidence, err := json.Marshal(request)
	if err != nil {
		writeError(w, badRequest(errors.New("encode startup recovery evidence")))
		return
	}
	if _, err := s.db.CompleteWorkerStartupRecovery(r.Context(), db.CompleteWorkerStartupRecoveryParams{
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true}, RecoveryEvidence: evidence,
	}); isNoRows(err) {
		writeError(w, conflict(errors.New("worker startup recovery fence is stale")))
		return
	} else if err != nil {
		s.log.Error("record worker startup recovery failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("record worker startup recovery"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateWorkerStartupRecovery(
	request workerapi.StartupRecoveryRequest,
	epochStartedAt time.Time,
	now time.Time,
) error {
	if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() {
		return errors.New("a complete, timestamped physical inventory is required")
	}
	if request.ObservedAt.After(now.Add(time.Minute)) {
		return errors.New("startup inventory observed_at is in the future")
	}
	if request.ObservedAt.Before(epochStartedAt) {
		return errors.New("startup inventory observed_at predates the current worker epoch")
	}
	inventory := make(map[uuid.UUID]struct{}, len(request.Inventory))
	for _, value := range request.Inventory {
		id, err := ids.Parse(value)
		if err != nil {
			return errors.New("inventory runtime id must be a canonical UUIDv7")
		}
		if _, exists := inventory[id]; exists {
			return fmt.Errorf("inventory runtime id %s is duplicated", id)
		}
		inventory[id] = struct{}{}
	}
	seen := make(map[uuid.UUID]string, len(request.Reclaimed)+len(request.Quarantined))
	validateIDs := func(kind string, values []string) error {
		for _, value := range values {
			id, err := ids.Parse(value)
			if err != nil {
				return fmt.Errorf("%s runtime id must be a canonical UUIDv7", kind)
			}
			if previous, exists := seen[id]; exists {
				return fmt.Errorf("runtime id %s is reported as both %s and %s", id, previous, kind)
			}
			if _, owned := inventory[id]; !owned {
				return fmt.Errorf("%s runtime id %s is outside the owned inventory", kind, id)
			}
			seen[id] = kind
		}
		return nil
	}
	if err := validateIDs("reclaimed", request.Reclaimed); err != nil {
		return err
	}
	if err := validateIDs("quarantined", request.Quarantined); err != nil {
		return err
	}
	if len(seen) != len(inventory) {
		return errors.New("every owned inventory runtime must have exactly one recovery outcome")
	}
	return nil
}

func (s *Server) workerObserve(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker observation storage is not configured")))
		return
	}
	var request workerapi.ObserveRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker observation JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if err := s.recordWorkerObservation(r.Context(), worker, request.Observation); err != nil {
		writeError(w, err)
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.DrainWorkerInstance(r.Context(), db.DrainWorkerInstanceParams{
		ID:                   pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:        worker.WorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		ExpectedClaimVersion: worker.ClaimVersion,
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("worker is not registered")))
		return
	} else if err != nil {
		s.log.Error("worker drain failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("drain worker"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerCompleteDrain(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request workerapi.DrainCompletionRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker drain completion JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() {
		writeError(w, badRequest(errors.New("a complete, timestamped physical inventory is required")))
		return
	}
	now := time.Now()
	if request.ObservedAt.After(now.Add(time.Minute)) {
		writeError(w, badRequest(errors.New("drain inventory observed_at is in the future")))
		return
	}
	if worker.EpochStartedAt.IsZero() || request.ObservedAt.Before(worker.EpochStartedAt) {
		writeError(w, badRequest(errors.New("drain inventory observed_at predates the current worker epoch")))
		return
	}
	if len(request.Inventory) != 0 || len(request.Reclaimed) != 0 || len(request.Quarantined) != 0 || len(request.Errors) != 0 {
		writeError(w, badRequest(errors.New("drain completion requires empty inventory, reclaimed, quarantined, and errors lists")))
		return
	}
	completed, err := s.completeWorkerDrain(r.Context(), db.CompleteWorkerDrainParams{
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:        worker.WorkerGroupID,
		WorkerEpoch:          pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		ExpectedClaimVersion: worker.ClaimVersion,
		ObservedAt:           pgvalue.Timestamptz(request.ObservedAt),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker drain is not complete or its claim fence is stale")))
		return
	}
	if err != nil {
		s.log.Error("worker drain completion failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("complete worker drain"))
		return
	}
	if completed.State != db.WorkerInstanceStateTerminationReady {
		writeError(w, errors.New("complete worker drain returned a non-terminal worker state"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StatusResponse{
		WorkerInstanceID: pgvalue.MustUUIDValue(completed.ID).String(),
		WorkerGroupID:    completed.WorkerGroupID,
		Status:           workerapi.StatusTerminationReady,
		ActiveExecutions: 0,
	})
}

func (s *Server) completeWorkerDrain(ctx context.Context, params db.CompleteWorkerDrainParams) (db.CompleteWorkerDrainRow, error) {
	var completed db.CompleteWorkerDrainRow
	err := s.inTx(ctx, func(work *txWork) error {
		if _, err := work.q.LockWorkerDrainCompletion(ctx, db.LockWorkerDrainCompletionParams{
			WorkerInstanceID: params.WorkerInstanceID,
			WorkerGroupID:    params.WorkerGroupID,
			WorkerEpoch:      params.WorkerEpoch,
		}); err != nil {
			return err
		}
		var err error
		completed, err = work.q.CompleteWorkerDrain(ctx, params)
		return err
	})
	return completed, err
}

func (s *Server) workerFence(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	var request workerapi.FenceRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker fence request JSON: %w", err)))
		return
	}
	reasonCode := strings.TrimSpace(request.ReasonCode)
	if reasonCode != "termination_drain_failed" && reasonCode != "worker_retired" {
		writeError(w, badRequest(errors.New("unsupported worker fence reason")))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.FenceWorkerInstance(r.Context(), db.FenceWorkerInstanceParams{
		ID:                   pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:        worker.WorkerGroupID,
		ExpectedEpoch:        pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		ExpectedClaimVersion: worker.ClaimVersion,
		ReasonCode:           pgtype.Text{String: reasonCode, Valid: true},
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("worker is not registered")))
		return
	} else if err != nil {
		s.log.Error("worker fence failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("fence worker"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	s.writeWorkerStatus(w, r, workerFromContext(r.Context()))
}

func (s *Server) writeWorkerStatus(w http.ResponseWriter, r *http.Request, worker workerActor) {
	state, err := s.db.GetWorkerInstanceState(r.Context(), db.GetWorkerInstanceStateParams{
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker is not registered")))
		return
	}
	if err != nil {
		s.log.Error("get worker status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("get worker status"))
		return
	}
	readiness := workerapi.Readiness{}
	if state.SupportsRun {
		readiness.Run = workerRoleReadiness(state, state.RunReady, state.RunPausedReason)
		readiness.Runtime = workerRoleReadiness(state, state.RuntimeReady, state.RuntimePausedReason)
	}
	if state.SupportsBuild {
		readiness.Build = workerRoleReadiness(state, state.BuildReady, state.BuildPausedReason)
	}
	writeJSON(w, http.StatusOK, workerapi.StatusResponse{
		WorkerInstanceID: pgvalue.MustUUIDValue(state.ID).String(),
		WorkerGroupID:    state.WorkerGroupID,
		Status:           workerapi.Status(state.State),
		ActiveExecutions: state.ActiveExecutions,
		Readiness:        readiness,
	})
}

func workerRoleReadiness(
	state db.GetWorkerInstanceStateRow,
	ready bool,
	pausedReason pgtype.Text,
) *workerapi.RoleReadiness {
	result := &workerapi.RoleReadiness{Ready: ready}
	if ready {
		return result
	}
	switch {
	case pausedReason.Valid:
		result.PausedReason = pausedReason.String
	case state.State != string(db.WorkerInstanceStateActive):
		result.PausedReason = "worker_not_active"
	case !state.ObservedAt.Valid:
		result.PausedReason = "observation_missing"
	default:
		result.PausedReason = "observation_stale"
	}
	return result
}

func (s *Server) recordWorkerObservation(ctx context.Context, worker workerActor, observation workerapi.Observation) error {
	if _, err := s.db.RecordWorkerObservation(
		ctx,
		workerObservationParams(worker, observation),
	); isNoRows(err) {
		return forbidden(errors.New("worker observation conflicts with this worker epoch"))
	} else if err != nil {
		return errors.New("record worker observation")
	}
	return nil
}

func workerObservationParams(worker workerActor, observation workerapi.Observation) db.RecordWorkerObservationParams {
	health := observation.HealthDetails
	if len(health) == 0 {
		health = json.RawMessage(`{}`)
	}
	return db.RecordWorkerObservationParams{
		CpuPressureBps: observation.CPUPressureBPS, MemoryPressureBps: observation.MemoryPressureBPS,
		GuestEphemeralDiskPressureBps: observation.GuestEphemeralDiskPressureBPS,
		BuildCachePressureBps:         observation.BuildCachePressureBPS, ArtifactCachePressureBps: observation.ArtifactCachePressureBPS,
		CheckpointPressureBps: observation.CheckpointPressureBPS, QuarantinedResourceCount: observation.QuarantinedResourceCount,
		RunQueueDepth: observation.RunQueueDepth, BuildQueueDepth: observation.BuildQueueDepth,
		RuntimeStartQueueDepth: observation.RuntimeStartQueueDepth,
		RunPausedReason:        pgtype.Text{String: observation.RunPausedReason, Valid: observation.RunPausedReason != ""},
		BuildPausedReason:      pgtype.Text{String: observation.BuildPausedReason, Valid: observation.BuildPausedReason != ""},
		RuntimePausedReason:    pgtype.Text{String: observation.RuntimePausedReason, Valid: observation.RuntimePausedReason != ""},
		HealthDetails:          health, ObservedAt: pgvalue.Timestamptz(time.Now()),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
	}
}

func workerActivationParams(
	worker workerActor,
	c workerapi.Capabilities,
) db.ActivateWorkerInstanceParams {
	supportsRun := c.SupportsRun
	maxRuntimeStarts := c.MaxRuntimeStarts
	if supportsRun && maxRuntimeStarts == 0 {
		maxRuntimeStarts = c.ExecutionSlotsAvailable
	}
	return db.ActivateWorkerInstanceParams{
		RuntimeIdentityID: c.RuntimeID, RuntimeArch: c.RuntimeArch, RuntimeABI: c.RuntimeABI,
		KernelDigest: c.KernelDigest, InitramfsDigest: c.InitramfsDigest, RootfsDigest: c.RootfsDigest,
		NetworkAbi: c.NetworkABI, ProtocolVersion: c.ProtocolVersion, SupervisorVersion: c.WorkerVersion,
		SupportsRun: supportsRun, SupportsBuild: c.SupportsBuild,
		SubstrateFormat: c.SubstrateFormat, SubstrateBuilderAbi: c.SubstrateBuilderABI, SubstrateLayoutAbi: c.SubstrateLayoutABI,
		EpochCpuMillis: c.MaxVCPUs * 1000, EpochMemoryBytes: c.MaxMemoryMiB * 1024 * 1024,
		EpochGuestEphemeralDiskBytes: c.GuestEphemeralDiskBytes,
		EpochBuildCacheBytes:         c.BuildCacheBytes, EpochArtifactCacheBytes: c.ArtifactCacheBytes,
		EpochHugepagesBytes: c.HugepagesBytes, EpochCheckpointBytes: c.CheckpointBytes,
		PerVmCpuMillis: c.VMMilliCPU, PerVmMemoryBytes: c.VMMemoryMiB * 1024 * 1024,
		PerVmGuestEphemeralDiskBytes: c.VMGuestEphemeralDiskBytes,
		MaxVmSlots:                   c.ExecutionSlotsAvailable, MaxRunConsumers: c.ExecutionSlotsAvailable,
		MaxBuildExecutors: c.MaxBuildExecutors, MaxRuntimeStarts: maxRuntimeStarts,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
	}
}

func normalizeWorkerCapabilities(input workerapi.Capabilities) (workerapi.Capabilities, error) {
	capabilities := workerapi.Capabilities{
		ProtocolVersion:           strings.TrimSpace(input.ProtocolVersion),
		WorkerVersion:             strings.TrimSpace(input.WorkerVersion),
		RuntimeID:                 strings.TrimSpace(input.RuntimeID),
		RuntimeArch:               strings.TrimSpace(input.RuntimeArch),
		RuntimeABI:                strings.TrimSpace(input.RuntimeABI),
		KernelDigest:              strings.TrimSpace(input.KernelDigest),
		InitramfsDigest:           strings.TrimSpace(input.InitramfsDigest),
		RootfsDigest:              strings.TrimSpace(input.RootfsDigest),
		NetworkABI:                strings.TrimSpace(input.NetworkABI),
		SubstrateFormat:           strings.TrimSpace(input.SubstrateFormat),
		SubstrateBuilderABI:       strings.TrimSpace(input.SubstrateBuilderABI),
		SubstrateLayoutABI:        strings.TrimSpace(input.SubstrateLayoutABI),
		MaxVCPUs:                  input.MaxVCPUs,
		MaxMemoryMiB:              input.MaxMemoryMiB,
		VMMilliCPU:                input.VMMilliCPU,
		VMMemoryMiB:               input.VMMemoryMiB,
		GuestEphemeralDiskBytes:   input.GuestEphemeralDiskBytes,
		VMGuestEphemeralDiskBytes: input.VMGuestEphemeralDiskBytes,
		ExecutionSlotsAvailable:   input.ExecutionSlotsAvailable,
		SupportsRun:               input.SupportsRun,
		SupportsBuild:             input.SupportsBuild,
		MaxBuildExecutors:         input.MaxBuildExecutors,
		MaxRuntimeStarts:          input.MaxRuntimeStarts,
		BuildCacheBytes:           input.BuildCacheBytes,
		ArtifactCacheBytes:        input.ArtifactCacheBytes,
		HugepagesBytes:            input.HugepagesBytes,
		CheckpointBytes:           input.CheckpointBytes,
		Observation:               input.Observation,
	}
	if capabilities.ProtocolVersion == "" {
		return workerapi.Capabilities{}, errors.New("worker protocol_version is required")
	}
	if capabilities.ProtocolVersion != workerapi.CurrentProtocolVersion {
		return workerapi.Capabilities{}, fmt.Errorf("worker protocol_version %s is not supported; current protocol is %s", capabilities.ProtocolVersion, workerapi.CurrentProtocolVersion)
	}
	if capabilities.RuntimeID == "" {
		return workerapi.Capabilities{}, errors.New("worker runtime_id is required")
	}
	if err := deployment.ValidateRuntimeArchitecture(deployment.RuntimeArchitecture(capabilities.RuntimeArch)); err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker runtime_arch: %w", err)
	}
	if capabilities.RuntimeABI == "" {
		return workerapi.Capabilities{}, errors.New("worker runtime_abi is required")
	}
	if capabilities.KernelDigest == "" {
		return workerapi.Capabilities{}, errors.New("worker kernel_digest is required")
	}
	if capabilities.InitramfsDigest == "" {
		return workerapi.Capabilities{}, errors.New("worker initramfs_digest is required")
	}
	if capabilities.RootfsDigest == "" {
		return workerapi.Capabilities{}, errors.New("worker rootfs_digest is required")
	}
	if capabilities.NetworkABI != workerapi.NetworkABIV0 {
		return workerapi.Capabilities{}, fmt.Errorf("worker network_abi must be %s", workerapi.NetworkABIV0)
	}
	expectedRuntimeID, err := runtimeid.Digest(runtimeid.Selector{
		Arch:            capabilities.RuntimeArch,
		ABI:             capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		NetworkABI:      capabilities.NetworkABI,
	})
	if err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker runtime identity: %w", err)
	}
	if capabilities.RuntimeID != expectedRuntimeID {
		return workerapi.Capabilities{}, fmt.Errorf("worker runtime_id %s does not match runtime identity %s", capabilities.RuntimeID, expectedRuntimeID)
	}
	if capabilities.MaxVCPUs <= 0 {
		return workerapi.Capabilities{}, errors.New("worker max_vcpus must be positive")
	}
	if capabilities.MaxVCPUs > math.MaxInt32 {
		return workerapi.Capabilities{}, fmt.Errorf("worker max_vcpus exceeds max %d", math.MaxInt32)
	}
	if capabilities.MaxMemoryMiB <= 0 {
		return workerapi.Capabilities{}, errors.New("worker max_memory_mib must be positive")
	}
	if capabilities.MaxMemoryMiB > math.MaxInt32 {
		return workerapi.Capabilities{}, fmt.Errorf("worker max_memory_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.VMMilliCPU <= 0 || capabilities.VMMilliCPU > capabilities.MaxVCPUs*1000 {
		return workerapi.Capabilities{}, errors.New("worker vm_milli_cpu must be positive and not exceed aggregate CPU")
	}
	if capabilities.VMMemoryMiB <= 0 || capabilities.VMMemoryMiB > capabilities.MaxMemoryMiB {
		return workerapi.Capabilities{}, errors.New("worker vm_memory_mib must be positive and not exceed aggregate memory")
	}
	if capabilities.GuestEphemeralDiskBytes <= 0 ||
		capabilities.VMGuestEphemeralDiskBytes <= 0 ||
		capabilities.VMGuestEphemeralDiskBytes > capabilities.GuestEphemeralDiskBytes {
		return workerapi.Capabilities{}, errors.New("worker VM guest ephemeral disk must be positive and not exceed aggregate capacity")
	}
	if capabilities.ExecutionSlotsAvailable <= 0 {
		return workerapi.Capabilities{}, errors.New("worker execution_slots_available must be positive")
	}
	if !capabilities.SupportsRun && !capabilities.SupportsBuild {
		return workerapi.Capabilities{}, errors.New("worker must support run, build, or both")
	}
	if capabilities.SupportsBuild && capabilities.MaxBuildExecutors != 1 {
		return workerapi.Capabilities{}, errors.New("worker max_build_executors must be one for build role")
	}
	if !capabilities.SupportsBuild && capabilities.MaxBuildExecutors != 0 {
		return workerapi.Capabilities{}, errors.New("worker max_build_executors must be zero without build role")
	}
	if capabilities.SupportsRun && capabilities.MaxRuntimeStarts <= 0 {
		return workerapi.Capabilities{}, errors.New("worker max_runtime_starts must be positive for run role")
	}
	if capabilities.SupportsRun {
		if capabilities.SubstrateFormat == "" {
			return workerapi.Capabilities{}, errors.New("worker substrate_format is required for run role")
		}
		if capabilities.SubstrateBuilderABI == "" {
			return workerapi.Capabilities{}, errors.New("worker substrate_builder_abi is required for run role")
		}
		if capabilities.SubstrateLayoutABI == "" {
			return workerapi.Capabilities{}, errors.New("worker substrate_layout_abi is required for run role")
		}
	} else if capabilities.SubstrateFormat != "" || capabilities.SubstrateBuilderABI != "" || capabilities.SubstrateLayoutABI != "" {
		return workerapi.Capabilities{}, errors.New("worker without run role must not report a substrate contract")
	}
	return capabilities, nil
}

func (s *Server) validateWorkerBuildPolicy(capabilities workerapi.Capabilities) error {
	if !capabilities.SupportsBuild {
		return nil
	}
	if s.buildPolicy == nil {
		return errors.New("build policy is not configured")
	}
	return nil
}
