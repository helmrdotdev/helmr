package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/capacity"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtimeid"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultWorkerTokenTTL = 15 * time.Minute

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
	if request.ResourceID == "" || strings.TrimSpace(request.ResourceID) != request.ResourceID || len(request.ResourceID) > 512 {
		writeError(w, badRequest(errors.New("resource_id is required and must not exceed 512 bytes")))
		return
	}
	if err := workergroup.ValidatePoolName(request.PoolName); err != nil {
		writeError(w, badRequest(fmt.Errorf("worker pool name: %w", err)))
		return
	}
	tokenHash, err := strictWorkerEnrollmentBearer(r.Header.Values("Authorization"))
	if err != nil {
		writeError(w, unauthorized(errors.New("worker enrollment token is invalid")))
		return
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authKeys.WorkerInstance)
	if err != nil {
		writeError(w, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := uuid.NewV7()
	credential, err := s.db.EnrollWorkerInstance(r.Context(), db.EnrollWorkerInstanceParams{
		TokenHash:        tokenHash,
		WorkerPoolID:     pgvalue.UUID(uuid.NewV7()),
		PoolName:         request.PoolName,
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		CurrentServiceID: pgvalue.UUID(uuid.NewV7()),
		ResourceID:       request.ResourceID,
		CredentialID:     pgvalue.UUID(uuid.NewV7()),
		KeyPrefix:        generated.KeyPrefix,
		SecretHash:       generated.TokenHash,
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker enrollment token is invalid")))
		return
	}
	if err != nil {
		s.log.Error("worker enrollment failed", "resource_id", request.ResourceID, "error", err)
		writeError(w, errors.New("enroll worker"))
		return
	}
	writeJSON(w, http.StatusCreated, workerapi.EnrollmentResponse{
		WorkerInstanceID:     pgvalue.MustUUIDValue(credential.WorkerInstanceID).String(),
		WorkerGroupID:        pgvalue.UUIDString(credential.WorkerGroupID),
		WorkerPoolID:         pgvalue.MustUUIDValue(credential.WorkerPoolID).String(),
		WorkerInstanceSecret: generated.Raw,
	})
}

func strictWorkerEnrollmentBearer(values []string) ([]byte, error) {
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return nil, errors.New("worker enrollment bearer is required")
	}
	raw := values[0][len("Bearer "):]
	if raw == "" || strings.ContainsAny(raw, " \t\r\n") {
		return nil, errors.New("worker enrollment bearer is invalid")
	}
	return workergroup.ParseEnrollmentToken(raw)
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
	credential, err := s.db.AuthenticateWorkerInstanceCredential(r.Context(), db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		SecretHash:       secretHash,
		ServiceID:        pgvalue.UUID(serviceID),
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
		WorkerGroupID:     pgvalue.MustUUIDValue(credential.WorkerGroupID),
		ClaimVersion:      credential.ClaimVersion,
		GroupClaimVersion: credential.GroupClaimVersion,
		WorkerEpoch:       credential.CurrentEpoch.Int64,
	}).Claims(auth.EpochExchangeInput{
		ServiceID: serviceID,
	}, now, expiresAt)
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
	worker := workerFromContext(r.Context())
	err = s.activateWorker(r.Context(), worker, capabilities)
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
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
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
		WorkerGroupID:        pgvalue.UUID(worker.WorkerGroupID),
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
		WorkerGroupID:        pgvalue.UUID(worker.WorkerGroupID),
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
		WorkerGroupID:    pgvalue.UUIDString(completed.WorkerGroupID),
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
		WorkerGroupID:        pgvalue.UUID(worker.WorkerGroupID),
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
		ID:                          pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:               pgvalue.UUID(worker.WorkerGroupID),
		ObservationFreshnessSeconds: workerapi.WorkerObservationFreshnessSeconds,
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
	readiness := workerapi.Readiness{
		Run:     workerRoleReadiness(state, state.RunReady, state.RunPausedReason),
		Runtime: workerRoleReadiness(state, state.RuntimeReady, state.RuntimePausedReason),
	}
	status, err := workerPublicStatus(state.State)
	if err != nil {
		s.log.Error("project worker status", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("project worker status"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StatusResponse{
		WorkerInstanceID: pgvalue.MustUUIDValue(state.ID).String(),
		WorkerGroupID:    pgvalue.UUIDString(state.WorkerGroupID),
		Status:           status,
		ActiveExecutions: state.ActiveExecutions,
		Readiness:        readiness,
	})
}

func workerPublicStatus(state string) (workerapi.Status, error) {
	switch state {
	case db.WorkerInstanceStateActive:
		return workerapi.StatusActive, nil
	case db.WorkerInstanceStateDraining:
		return workerapi.StatusDraining, nil
	case db.WorkerInstanceStateTerminationReady:
		return workerapi.StatusTerminationReady, nil
	default:
		return "", fmt.Errorf("worker instance state %q has no Worker projection", state)
	}
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
	return db.RecordWorkerObservationParams{
		RunPausedReason:     pgtype.Text{String: observation.RunPausedReason, Valid: observation.RunPausedReason != ""},
		RuntimePausedReason: pgtype.Text{String: observation.RuntimePausedReason, Valid: observation.RuntimePausedReason != ""},
		WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
	}
}

func workerActivationParams(
	worker workerActor,
	c workerapi.Capabilities,
	cpuEnvironment []byte,
) db.ActivateWorkerInstanceParams {
	return db.ActivateWorkerInstanceParams{
		RuntimeIdentityID: pgtype.Text{String: c.Runtime.ID, Valid: true},
		SubstrateFormat:   c.SubstrateFormat, SubstrateContract: c.SubstrateContract,
		EpochCPUMillis: c.MaxVCPUs * 1000, EpochMemoryBytes: c.MaxMemoryMiB * 1024 * 1024,
		EpochGuestEphemeralDiskBytes: c.GuestEphemeralDiskBytes,
		PerVMCPUMillis:               c.VMMilliCPU, PerVMMemoryBytes: c.VMMemoryMiB * 1024 * 1024,
		PerVMGuestEphemeralDiskBytes: c.VMGuestEphemeralDiskBytes,
		MaxVMSlots:                   c.ExecutionSlotsAvailable,
		MaxRuntimeStarts:             c.ExecutionSlotsAvailable,
		CPUEnvironment:               cpuEnvironment, CPUEnvironmentDigest: pgtype.Text{String: c.CPUEnvironment.Digest, Valid: true},
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
	}
}

func (s *Server) activateWorker(ctx context.Context, worker workerActor, capabilities workerapi.Capabilities) error {
	cpuEnvironment, err := json.Marshal(capabilities.CPUEnvironment)
	if err != nil {
		return fmt.Errorf("encode Worker CPU environment: %w", err)
	}
	template := workerTemplate(capabilities)
	return s.inTx(ctx, func(work *txWork) error {
		group, err := work.q.LockWorkerGroupForPoolMutation(ctx, pgvalue.UUID(worker.WorkerGroupID))
		if err != nil {
			return err
		}
		if group.State != db.WorkerGroupStateActive && group.State != db.WorkerGroupStatePaused && group.State != db.WorkerGroupStateDraining {
			return pgx.ErrNoRows
		}
		epoch := pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true}
		poolID, err := work.q.GetWorkerInstancePoolID(ctx, db.GetWorkerInstancePoolIDParams{
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
			WorkerEpoch: epoch,
		})
		if err != nil {
			return err
		}
		pool, err := work.q.LockWorkerPool(ctx, db.LockWorkerPoolParams{
			WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerPoolID: poolID,
		})
		if err != nil {
			return err
		}
		if _, err := work.q.LockWorkerInstanceForActivation(ctx, db.LockWorkerInstanceForActivationParams{
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID),
			WorkerPoolID: poolID, WorkerEpoch: epoch,
		}); err != nil {
			return err
		}
		if _, err := work.q.UpsertRuntimeIdentity(ctx, runtimeIdentityParams(capabilities.Runtime)); err != nil {
			return err
		}
		switch pool.State {
		case "pending":
			if group.State == db.WorkerGroupStateDraining {
				return pgx.ErrNoRows
			}
			for _, shape := range capabilities.CPUShapes {
				inserted, err := work.q.InsertWorkerPoolCPUShape(ctx, db.InsertWorkerPoolCPUShapeParams{
					VCPUCount: shape.VCPUCount, CPUConfigDigest: shape.CPUConfigDigest, WorkerPoolID: poolID,
				})
				if err != nil {
					return err
				}
				if inserted != 1 {
					return pgx.ErrNoRows
				}
			}
			pool, err = work.q.SealWorkerPool(ctx, sealWorkerPoolParams(worker.WorkerGroupID, poolID, template))
			if err != nil {
				return err
			}
			if _, err := work.q.SetInitialWorkerGroupPrimaryPool(ctx, db.SetInitialWorkerGroupPrimaryPoolParams{
				WorkerGroupID: pgvalue.UUID(worker.WorkerGroupID), WorkerPoolID: poolID,
			}); err != nil {
				return err
			}
		case "active", "draining":
			shapes, err := work.q.ListWorkerPoolCPUShapes(ctx, poolID)
			if err != nil {
				return err
			}
			if !workerPoolMatches(pool, shapes, template) {
				return pgx.ErrNoRows
			}
		default:
			return pgx.ErrNoRows
		}
		_, err = work.q.ActivateWorkerInstance(ctx, workerActivationParams(worker, capabilities, cpuEnvironment))
		return err
	})
}

func runtimeIdentityParams(profile runtimeid.Profile) db.UpsertRuntimeIdentityParams {
	digest := pgtype.Text{String: profile.CPUTemplate.Digest, Valid: profile.CPUTemplate.Digest != ""}
	return db.UpsertRuntimeIdentityParams{
		ID: profile.ID, RuntimeArch: profile.Arch, VMRuntimeContract: profile.Contract,
		VMRuntimeDescriptorDigest: profile.VMRuntimeDescriptorDigest,
		FirecrackerDigest:         profile.FirecrackerDigest, FirecrackerVersion: profile.FirecrackerVersion,
		SnapshotFormatVersion: profile.SnapshotFormatVersion, HostKernelRelease: profile.HostKernelRelease,
		CPUTemplateKind: string(profile.CPUTemplate.Kind), CPUTemplateDigest: digest,
		KernelDigest: profile.KernelDigest, InitramfsDigest: profile.InitramfsDigest, RootfsDigest: profile.RootfsDigest,
	}
}

func sealWorkerPoolParams(groupID uuid.UUID, poolID pgtype.UUID, template capacity.WorkerTemplate) db.SealWorkerPoolParams {
	return db.SealWorkerPoolParams{
		RuntimeIdentityID:               pgtype.Text{String: template.Runtime.ID, Valid: true},
		SubstrateFormat:                 pgtype.Text{String: template.Substrate.Format, Valid: true},
		SubstrateContract:               pgtype.Text{String: template.Substrate.Contract, Valid: true},
		CapacityCPUMillis:               pgtype.Int8{Int64: template.Capacity.CPUMillis, Valid: true},
		CapacityMemoryBytes:             pgtype.Int8{Int64: template.Capacity.MemoryBytes, Valid: true},
		CapacityGuestEphemeralDiskBytes: pgtype.Int8{Int64: template.Capacity.GuestEphemeralDiskBytes, Valid: true},
		PerVMCPUMillis:                  pgtype.Int8{Int64: template.PerVM.CPUMillis, Valid: true},
		PerVMMemoryBytes:                pgtype.Int8{Int64: template.PerVM.MemoryBytes, Valid: true},
		PerVMGuestEphemeralDiskBytes:    pgtype.Int8{Int64: template.PerVM.GuestEphemeralDiskBytes, Valid: true},
		MaxVMSlots:                      pgtype.Int4{Int32: int32(template.Capacity.VMSlots), Valid: true},
		WorkerPoolID:                    poolID, WorkerGroupID: pgvalue.UUID(groupID),
	}
}

func workerPoolMatches(pool db.WorkerPool, shapes []db.WorkerPoolCpuShape, template capacity.WorkerTemplate) bool {
	if !pool.SealedAt.Valid ||
		!pool.RuntimeIdentityID.Valid || pool.RuntimeIdentityID.String != template.Runtime.ID ||
		!pool.SubstrateFormat.Valid || pool.SubstrateFormat.String != template.Substrate.Format ||
		!pool.SubstrateContract.Valid || pool.SubstrateContract.String != template.Substrate.Contract ||
		!pool.CapacityCPUMillis.Valid || pool.CapacityCPUMillis.Int64 != template.Capacity.CPUMillis ||
		!pool.CapacityMemoryBytes.Valid || pool.CapacityMemoryBytes.Int64 != template.Capacity.MemoryBytes ||
		!pool.CapacityGuestEphemeralDiskBytes.Valid || pool.CapacityGuestEphemeralDiskBytes.Int64 != template.Capacity.GuestEphemeralDiskBytes ||
		!pool.PerVMCPUMillis.Valid || pool.PerVMCPUMillis.Int64 != template.PerVM.CPUMillis ||
		!pool.PerVMMemoryBytes.Valid || pool.PerVMMemoryBytes.Int64 != template.PerVM.MemoryBytes ||
		!pool.PerVMGuestEphemeralDiskBytes.Valid || pool.PerVMGuestEphemeralDiskBytes.Int64 != template.PerVM.GuestEphemeralDiskBytes ||
		!pool.MaxVMSlots.Valid || int64(pool.MaxVMSlots.Int32) != template.Capacity.VMSlots ||
		len(shapes) != len(template.CPUShapes) {
		return false
	}
	for index := range shapes {
		if shapes[index].VCPUCount != template.CPUShapes[index].VCPUCount ||
			shapes[index].CPUConfigDigest != template.CPUShapes[index].CPUConfigDigest {
			return false
		}
	}
	return true
}

func workerTemplate(capabilities workerapi.Capabilities) capacity.WorkerTemplate {
	return capacity.WorkerTemplate{
		Schema: capacity.WorkerTemplateSchema, Runtime: capabilities.Runtime,
		CPUShapes: append([]runtimeid.CPUShape(nil), capabilities.CPUShapes...),
		Substrate: capacity.SubstrateProfile{
			Format: capabilities.SubstrateFormat, Contract: capabilities.SubstrateContract,
		},
		Capacity: capacity.ResourceVector{
			CPUMillis: capabilities.MaxVCPUs * 1000, MemoryBytes: capabilities.MaxMemoryMiB * 1024 * 1024,
			GuestEphemeralDiskBytes: capabilities.GuestEphemeralDiskBytes,
			VMSlots:                 int64(capabilities.ExecutionSlotsAvailable),
		},
		PerVM: capacity.ResourceVector{
			CPUMillis: capabilities.VMMilliCPU, MemoryBytes: capabilities.VMMemoryMiB * 1024 * 1024,
			GuestEphemeralDiskBytes: capabilities.VMGuestEphemeralDiskBytes,
		},
	}
}

func normalizeWorkerCapabilities(input workerapi.Capabilities) (workerapi.Capabilities, error) {
	runtimeProfile := input.Runtime
	runtimeProfile.ID = strings.TrimSpace(runtimeProfile.ID)
	runtimeProfile.Arch = strings.TrimSpace(runtimeProfile.Arch)
	runtimeProfile.Contract = strings.TrimSpace(runtimeProfile.Contract)
	runtimeProfile.VMRuntimeDescriptorDigest = strings.TrimSpace(runtimeProfile.VMRuntimeDescriptorDigest)
	runtimeProfile.FirecrackerDigest = strings.TrimSpace(runtimeProfile.FirecrackerDigest)
	runtimeProfile.FirecrackerVersion = strings.TrimSpace(runtimeProfile.FirecrackerVersion)
	runtimeProfile.SnapshotFormatVersion = strings.TrimSpace(runtimeProfile.SnapshotFormatVersion)
	runtimeProfile.HostKernelRelease = strings.TrimSpace(runtimeProfile.HostKernelRelease)
	runtimeProfile.CPUTemplate.Digest = strings.TrimSpace(runtimeProfile.CPUTemplate.Digest)
	runtimeProfile.KernelDigest = strings.TrimSpace(runtimeProfile.KernelDigest)
	runtimeProfile.InitramfsDigest = strings.TrimSpace(runtimeProfile.InitramfsDigest)
	runtimeProfile.RootfsDigest = strings.TrimSpace(runtimeProfile.RootfsDigest)
	cpuShapes := append([]runtimeid.CPUShape(nil), input.CPUShapes...)
	for index := range cpuShapes {
		cpuShapes[index].CPUConfigDigest = strings.TrimSpace(cpuShapes[index].CPUConfigDigest)
	}
	cpuEnvironment := input.CPUEnvironment
	cpuEnvironment.Digest = strings.TrimSpace(cpuEnvironment.Digest)
	cpuEnvironment.FirecrackerVersion = strings.TrimSpace(cpuEnvironment.FirecrackerVersion)
	cpuEnvironment.HostKernelRelease = strings.TrimSpace(cpuEnvironment.HostKernelRelease)
	cpuEnvironment.MicrocodeVersion = strings.TrimSpace(cpuEnvironment.MicrocodeVersion)
	cpuEnvironment.BIOSVersion = strings.TrimSpace(cpuEnvironment.BIOSVersion)
	cpuEnvironment.BIOSRevision = strings.TrimSpace(cpuEnvironment.BIOSRevision)
	capabilities := workerapi.Capabilities{
		Runtime:                   runtimeProfile,
		CPUShapes:                 cpuShapes,
		CPUEnvironment:            cpuEnvironment,
		SubstrateFormat:           strings.TrimSpace(input.SubstrateFormat),
		SubstrateContract:         strings.TrimSpace(input.SubstrateContract),
		MaxVCPUs:                  input.MaxVCPUs,
		MaxMemoryMiB:              input.MaxMemoryMiB,
		VMMilliCPU:                input.VMMilliCPU,
		VMMemoryMiB:               input.VMMemoryMiB,
		GuestEphemeralDiskBytes:   input.GuestEphemeralDiskBytes,
		VMGuestEphemeralDiskBytes: input.VMGuestEphemeralDiskBytes,
		ExecutionSlotsAvailable:   input.ExecutionSlotsAvailable,
	}
	if err := capabilities.Runtime.Validate(); err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker runtime profile: %w", err)
	}
	if err := capabilities.CPUEnvironment.Validate(); err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker CPU environment: %w", err)
	}
	if capabilities.CPUEnvironment.FirecrackerVersion != capabilities.Runtime.FirecrackerVersion ||
		capabilities.CPUEnvironment.HostKernelRelease != capabilities.Runtime.HostKernelRelease {
		return workerapi.Capabilities{}, errors.New("worker CPU environment does not match the runtime profile")
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
	if capabilities.SubstrateFormat == "" {
		return workerapi.Capabilities{}, errors.New("worker substrate_format is required")
	}
	if capabilities.SubstrateContract == "" {
		return workerapi.Capabilities{}, errors.New("worker substrate_contract is required")
	}
	template := workerTemplate(capabilities)
	if err := template.Validate(); err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker template: %w", err)
	}
	return capabilities, nil
}
