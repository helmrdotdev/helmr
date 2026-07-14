package control

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const defaultWorkerTokenTTL = 15 * time.Minute

const workerEnrollmentNonceTTL = 2 * time.Minute

type VerifiedWorkerEnrollment struct {
	WorkerGroupID               string
	ResourceID                  string
	AllowsRun                   bool
	AllowsBuild                 bool
	ProtocolVersion             string
	EnrollmentPolicyFingerprint string
	AttestationFingerprint      string
}

type WorkerEnrollmentVerifier interface {
	VerifyWorkerEnrollment(context.Context, api.WorkerEnrollmentRequest) (VerifiedWorkerEnrollment, error)
}

func (s *Server) workerEnrollmentChallenge(w http.ResponseWriter, r *http.Request) {
	if !s.workerEnrollmentGuard.allowChallenge(workerEnrollmentSource(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, tooManyRequests(errors.New("worker enrollment challenge rate limit exceeded")))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	var request api.WorkerEnrollmentChallengeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker enrollment challenge JSON: %w", err)))
		return
	}
	request.WorkerGroupID = strings.TrimSpace(request.WorkerGroupID)
	if request.WorkerGroupID == "" {
		writeError(w, badRequest(errors.New("worker_group_id is required")))
		return
	}
	rawNonce := make([]byte, 32)
	if _, err := rand.Read(rawNonce); err != nil {
		writeError(w, errors.New("generate worker enrollment challenge"))
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(rawNonce)
	nonceHash, err := auth.HashToken(s.authSecret, nonce)
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
	writeJSON(w, http.StatusCreated, api.WorkerEnrollmentChallengeResponse{
		Nonce:           nonce,
		WorkerGroupID:   created.WorkerGroupID,
		ExpiresAt:       pgvalue.Time(created.ExpiresAt),
		ProtocolVersion: auth.WorkerProtocolVersion,
	})
}

func (s *Server) workerEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.workerEnrollmentGuard.allowEnrollment(workerEnrollmentSource(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, tooManyRequests(errors.New("worker enrollment rate limit exceeded")))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request api.WorkerEnrollmentRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker enrollment JSON: %w", err)))
		return
	}
	request.WorkerGroupID = strings.TrimSpace(request.WorkerGroupID)
	request.Nonce = strings.TrimSpace(request.Nonce)
	request.ProtocolVersion = strings.TrimSpace(request.ProtocolVersion)
	if request.WorkerGroupID == "" || request.ProtocolVersion != auth.WorkerProtocolVersion || (!request.SupportsRun && !request.SupportsBuild) {
		writeError(w, badRequest(errors.New("worker_group_id, protocol_version, and at least one supported role are required")))
		return
	}
	if request.Nonce == "" {
		writeError(w, badRequest(errors.New("nonce is required")))
		return
	}
	if len(request.InstanceIdentityDocument) == 0 || len(request.InstanceIdentityDocument) > 16<<10 || len(request.SignedSTSRequest.Body) > 4<<10 || len(request.SignedSTSRequest.Headers) > 32 {
		writeError(w, badRequest(errors.New("worker enrollment evidence is missing or too large")))
		return
	}
	nonceHash, err := auth.HashToken(s.authSecret, request.Nonce)
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
	verified, err := s.workerEnrollment.VerifyWorkerEnrollment(r.Context(), request)
	if err != nil {
		s.log.Warn("worker enrollment evidence rejected", "worker_group_id", request.WorkerGroupID, "error", err)
		writeError(w, unauthorized(errors.New("worker enrollment evidence is invalid")))
		return
	}
	if verified.WorkerGroupID != request.WorkerGroupID || strings.TrimSpace(verified.ResourceID) == "" || strings.TrimSpace(verified.EnrollmentPolicyFingerprint) == "" || strings.TrimSpace(verified.AttestationFingerprint) == "" || (!verified.AllowsRun && !verified.AllowsBuild) || verified.ProtocolVersion != auth.WorkerProtocolVersion || (request.SupportsRun && !verified.AllowsRun) || (request.SupportsBuild && !verified.AllowsBuild) {
		writeError(w, unauthorized(errors.New("worker enrollment identity is invalid")))
		return
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authSecret)
	if err != nil {
		writeError(w, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := uuid.Must(uuid.NewV7())
	credential, err := s.db.EnrollWorkerInstance(r.Context(), db.EnrollWorkerInstanceParams{
		NonceHash:                   nonceHash,
		WorkerGroupID:               verified.WorkerGroupID,
		AllowsRun:                   request.SupportsRun,
		AllowsBuild:                 request.SupportsBuild,
		ProtocolVersion:             verified.ProtocolVersion,
		EnrollmentPolicyFingerprint: verified.EnrollmentPolicyFingerprint,
		WorkerInstanceID:            pgvalue.UUID(workerInstanceID),
		ResourceID:                  verified.ResourceID,
		CredentialID:                pgvalue.UUID(uuid.Must(uuid.NewV7())),
		KeyPrefix:                   generated.KeyPrefix,
		SecretHash:                  generated.TokenHash,
		AttestationFingerprint:      verified.AttestationFingerprint,
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker enrollment challenge is invalid or expired")))
		return
	}
	if err != nil {
		s.log.Error("worker enrollment failed", "worker_group_id", verified.WorkerGroupID, "resource_id", verified.ResourceID, "error", err)
		writeError(w, errors.New("enroll worker"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerEnrollmentResponse{
		WorkerInstanceID:     pgvalue.MustUUIDValue(credential.WorkerInstanceID).String(),
		WorkerGroupID:        credential.WorkerGroupID,
		WorkerInstanceSecret: generated.Raw,
	})
}

func (s *Server) workerAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil || len(s.authSecret) == 0 || len(s.workerTokenSecret) == 0 {
		writeError(w, unavailable(errors.New("worker authentication is not configured")))
		return
	}
	var request api.WorkerTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker token request JSON: %w", err)))
		return
	}
	request.WorkerInstanceID = strings.TrimSpace(request.WorkerInstanceID)
	if request.WorkerInstanceID == "" {
		writeError(w, badRequest(errors.New("worker_instance_id is required")))
		return
	}
	workerInstanceID, err := uuid.Parse(request.WorkerInstanceID)
	if err != nil {
		writeError(w, badRequest(errors.New("worker_instance_id must be a UUID")))
		return
	}
	secretHash, err := auth.HashToken(s.authSecret, request.WorkerInstanceSecret)
	if err != nil {
		writeError(w, unauthorized(errors.New("worker authentication is required")))
		return
	}
	serviceID, err := uuid.Parse(strings.TrimSpace(request.ServiceID))
	if err != nil {
		writeError(w, badRequest(errors.New("service_id must be a UUID")))
		return
	}
	protocolVersion := strings.TrimSpace(request.ProtocolVersion)
	if protocolVersion != auth.WorkerProtocolVersion {
		writeError(w, badRequest(fmt.Errorf("protocol_version must be %s", auth.WorkerProtocolVersion)))
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
	signed, err := auth.IssueWorkerToken(s.workerTokenSecret, claims)
	if err != nil {
		s.log.Error("mint worker token failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerTokenResponse{
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
	var request api.WorkerActivateRequest
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
	if strings.TrimSpace(request.CertificationProfile) == "" {
		request.CertificationProfile = "helmr-runtime-v0"
	}
	if strings.TrimSpace(request.CertificationFingerprint) == "" {
		request.CertificationFingerprint = capabilities.RuntimeID
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.CertifyWorkerInstance(r.Context(), workerCertificationParams(worker, request, capabilities)); isNoRows(err) {
		writeError(w, conflict(errors.New("worker certification is stale")))
		return
	} else if err != nil {
		s.log.Error("worker activate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("certify worker"))
		return
	}
	if err := s.recordWorkerObservation(r.Context(), worker, capabilities.Observation); err != nil {
		writeError(w, err)
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerStartupRecovery(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker recovery storage is not configured")))
		return
	}
	var request api.WorkerStartupRecoveryRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker startup recovery JSON: %w", err)))
		return
	}
	if !request.InventoryComplete || request.InventoryScope != "worker_runtime_state_roots_v0" || request.ObservedAt.IsZero() {
		writeError(w, badRequest(errors.New("a complete, timestamped physical inventory is required")))
		return
	}
	if request.ObservedAt.After(time.Now().Add(time.Minute)) {
		writeError(w, badRequest(errors.New("startup inventory observed_at is in the future")))
		return
	}
	inventory := make(map[uuid.UUID]struct{}, len(request.Inventory))
	for _, value := range request.Inventory {
		id, err := uuid.Parse(strings.TrimSpace(value))
		if err != nil {
			writeError(w, badRequest(errors.New("inventory runtime id must be a UUID")))
			return
		}
		if _, exists := inventory[id]; exists {
			writeError(w, badRequest(fmt.Errorf("inventory runtime id %s is duplicated", id)))
			return
		}
		inventory[id] = struct{}{}
	}
	seen := make(map[uuid.UUID]string, len(request.Reclaimed)+len(request.Quarantined))
	validateIDs := func(kind string, values []string) error {
		for _, value := range values {
			id, err := uuid.Parse(strings.TrimSpace(value))
			if err != nil {
				return fmt.Errorf("%s runtime id must be a UUID", kind)
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
		writeError(w, badRequest(err))
		return
	}
	if err := validateIDs("quarantined", request.Quarantined); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if len(seen) != len(inventory) {
		writeError(w, badRequest(errors.New("every owned inventory runtime must have exactly one recovery outcome")))
		return
	}
	evidence, err := json.Marshal(request)
	if err != nil {
		writeError(w, badRequest(errors.New("encode startup recovery evidence")))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.RecordWorkerStartupRecovery(r.Context(), db.RecordWorkerStartupRecoveryParams{
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

func (s *Server) workerObserve(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker observation storage is not configured")))
		return
	}
	var request api.WorkerObserveRequest
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

func (s *Server) workerRenewCertification(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker storage is not configured")))
		return
	}
	var request api.WorkerCertificationRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker certification renewal JSON: %w", err)))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.RenewWorkerCertification(r.Context(), workerCertificationRenewParams(worker, capabilities)); isNoRows(err) {
		writeError(w, conflict(errors.New("worker certification facts changed")))
		return
	} else if err != nil {
		writeError(w, errors.New("renew worker certification"))
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func workerCertificationRenewParams(worker workerActor, c api.WorkerCapabilities) db.RenewWorkerCertificationParams {
	return db.RenewWorkerCertificationParams{
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID, WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		RuntimeIdentityID: c.RuntimeID, ProtocolVersion: c.ProtocolVersion, SupportsRun: c.SupportsRun, SupportsBuild: c.SupportsBuild,
		CertifiedCpuMillis: c.MaxVCPUs * 1000, CertifiedMemoryBytes: c.MaxMemoryMiB * 1024 * 1024,
		CertifiedWorkloadDiskBytes: c.MaxDiskMiB * 1024 * 1024, CertifiedScratchBytes: c.ScratchBytes,
		CertifiedBuildCacheBytes: c.BuildCacheBytes, CertifiedArtifactCacheBytes: c.ArtifactCacheBytes,
		CertifiedHugepagesBytes: c.HugepagesBytes, CertifiedCheckpointBytes: c.CheckpointBytes,
		PerVmCpuMillis: c.VMMilliCPU, PerVmMemoryBytes: c.VMMemoryMiB * 1024 * 1024,
		PerVmWorkloadDiskBytes: c.VMMaxDiskMiB * 1024 * 1024, PerVmScratchBytes: c.VMMaxScratchBytes,
		MaxVmSlots: c.ExecutionSlotsAvailable, MaxBuildExecutors: c.MaxBuildExecutors, MaxRuntimeStarts: c.MaxRuntimeStarts,
	}
}

func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.DrainWorkerInstance(r.Context(), db.DrainWorkerInstanceParams{
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
		ExpectedEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
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

type canonicalWorkerDrainCleanupEvidence struct {
	InventoryComplete bool      `json:"inventory_complete"`
	InventoryScope    string    `json:"inventory_scope"`
	ObservedAt        time.Time `json:"observed_at"`
	Inventory         []string  `json:"inventory"`
	Reclaimed         []string  `json:"reclaimed"`
	Quarantined       []string  `json:"quarantined"`
	Errors            []string  `json:"errors"`
}

func (s *Server) workerCompleteDrain(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request api.WorkerDrainCompletionRequest
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
	evidence, err := json.Marshal(canonicalWorkerDrainCleanupEvidence{
		InventoryComplete: true,
		InventoryScope:    "worker_runtime_state_roots_v0",
		ObservedAt:        request.ObservedAt.UTC(),
		Inventory:         []string{},
		Reclaimed:         []string{},
		Quarantined:       []string{},
		Errors:            []string{},
	})
	if err != nil {
		writeError(w, errors.New("encode worker drain cleanup evidence"))
		return
	}
	if len(evidence) > 16<<10 {
		writeError(w, badRequest(errors.New("worker drain cleanup evidence is too large")))
		return
	}
	sum := sha256.Sum256(evidence)
	fingerprint := hex.EncodeToString(sum[:])
	completed, err := s.completeWorkerDrain(r.Context(), db.CompleteWorkerDrainParams{
		WorkerInstanceID:   pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:      worker.WorkerGroupID,
		WorkerEpoch:        pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		CleanupFingerprint: pgtype.Text{String: fingerprint, Valid: true},
		CleanupEvidence:    evidence,
		ObservedAt:         pgvalue.Timestamptz(request.ObservedAt),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker drain is not complete or cleanup proof conflicts with the recorded proof")))
		return
	}
	if err != nil {
		s.log.Error("worker drain completion failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("complete worker drain"))
		return
	}
	if completed.State != db.WorkerInstanceStateDisabled {
		writeError(w, errors.New("complete worker drain returned a non-terminal worker state"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerStatusResponse{
		WorkerInstanceID: pgvalue.MustUUIDValue(completed.ID).String(),
		WorkerGroupID:    completed.WorkerGroupID,
		Status:           api.WorkerStatusDisabled,
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
	var request api.WorkerFenceRequest
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
		ID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID: worker.WorkerGroupID,
		ExpectedEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
		ReasonCode:    pgtype.Text{String: reasonCode, Valid: true},
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
	writeJSON(w, http.StatusOK, api.WorkerStatusResponse{
		WorkerInstanceID: pgvalue.MustUUIDValue(state.ID).String(),
		WorkerGroupID:    state.WorkerGroupID,
		Status:           api.WorkerStatus(state.State),
		ActiveExecutions: state.ActiveExecutions,
	})
}

func (s *Server) recordWorkerObservation(ctx context.Context, worker workerActor, observation api.WorkerObservation) error {
	if _, err := s.db.RecordWorkerObservation(ctx, workerObservationParams(worker, observation)); isNoRows(err) {
		return forbidden(errors.New("worker observation conflicts with this worker epoch"))
	} else if err != nil {
		return errors.New("record worker observation")
	}
	return nil
}

func workerObservationParams(worker workerActor, observation api.WorkerObservation) db.RecordWorkerObservationParams {
	health := observation.HealthDetails
	if len(health) == 0 {
		health = json.RawMessage(`{}`)
	}
	return db.RecordWorkerObservationParams{
		CpuPressureBps: observation.CPUPressureBPS, MemoryPressureBps: observation.MemoryPressureBPS,
		WorkloadDiskPressureBps: observation.WorkloadDiskPressureBPS, ScratchPressureBps: observation.ScratchPressureBPS,
		BuildCachePressureBps: observation.BuildCachePressureBPS, ArtifactCachePressureBps: observation.ArtifactCachePressureBPS,
		CheckpointPressureBps: observation.CheckpointPressureBPS, LeakedSlotCount: observation.LeakedSlotCount,
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

func workerCertificationParams(worker workerActor, request api.WorkerActivateRequest, c api.WorkerCapabilities) db.CertifyWorkerInstanceParams {
	supportsRun := c.SupportsRun
	maxRuntimeStarts := c.MaxRuntimeStarts
	if supportsRun && maxRuntimeStarts == 0 {
		maxRuntimeStarts = c.ExecutionSlotsAvailable
	}
	return db.CertifyWorkerInstanceParams{
		RuntimeIdentityID: c.RuntimeID, RuntimeArch: c.RuntimeArch, RuntimeABI: c.RuntimeABI,
		KernelDigest: c.KernelDigest, InitramfsDigest: c.InitramfsDigest, RootfsDigest: c.RootfsDigest,
		CniProfile: c.CNIProfile, ProtocolVersion: c.ProtocolVersion, SupervisorVersion: c.WorkerVersion,
		SupportsRun: supportsRun, SupportsBuild: c.SupportsBuild,
		CertifiedCpuMillis: c.MaxVCPUs * 1000, CertifiedMemoryBytes: c.MaxMemoryMiB * 1024 * 1024,
		CertifiedWorkloadDiskBytes: c.MaxDiskMiB * 1024 * 1024, CertifiedScratchBytes: c.ScratchBytes,
		CertifiedBuildCacheBytes: c.BuildCacheBytes, CertifiedArtifactCacheBytes: c.ArtifactCacheBytes,
		CertifiedHugepagesBytes: c.HugepagesBytes, CertifiedCheckpointBytes: c.CheckpointBytes,
		PerVmCpuMillis: c.VMMilliCPU, PerVmMemoryBytes: c.VMMemoryMiB * 1024 * 1024,
		PerVmWorkloadDiskBytes: c.VMMaxDiskMiB * 1024 * 1024, PerVmScratchBytes: c.VMMaxScratchBytes,
		MaxVmSlots: c.ExecutionSlotsAvailable, MaxRunConsumers: c.ExecutionSlotsAvailable,
		MaxBuildExecutors: c.MaxBuildExecutors, MaxRuntimeStarts: maxRuntimeStarts,
		CertificationProfile: request.CertificationProfile, CertificationFingerprint: request.CertificationFingerprint,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID), WorkerGroupID: worker.WorkerGroupID,
		WorkerEpoch: pgtype.Int8{Int64: worker.WorkerEpoch, Valid: true},
	}
}

func normalizeWorkerCapabilities(input api.WorkerCapabilities) (api.WorkerCapabilities, error) {
	capabilities := api.WorkerCapabilities{
		ProtocolVersion:         strings.TrimSpace(input.ProtocolVersion),
		WorkerVersion:           strings.TrimSpace(input.WorkerVersion),
		RuntimeID:               strings.TrimSpace(input.RuntimeID),
		RuntimeArch:             strings.TrimSpace(input.RuntimeArch),
		RuntimeABI:              strings.TrimSpace(input.RuntimeABI),
		KernelDigest:            strings.TrimSpace(input.KernelDigest),
		InitramfsDigest:         strings.TrimSpace(input.InitramfsDigest),
		RootfsDigest:            strings.TrimSpace(input.RootfsDigest),
		CNIProfile:              strings.TrimSpace(input.CNIProfile),
		Region:                  strings.TrimSpace(input.Region),
		MaxVCPUs:                input.MaxVCPUs,
		MaxMemoryMiB:            input.MaxMemoryMiB,
		VMMilliCPU:              input.VMMilliCPU,
		VMMemoryMiB:             input.VMMemoryMiB,
		MaxDiskMiB:              input.MaxDiskMiB,
		VMMaxDiskMiB:            input.VMMaxDiskMiB,
		ExecutionSlotsAvailable: input.ExecutionSlotsAvailable,
		SupportsRun:             input.SupportsRun,
		SupportsBuild:           input.SupportsBuild,
		MaxBuildExecutors:       input.MaxBuildExecutors,
		MaxRuntimeStarts:        input.MaxRuntimeStarts,
		ScratchBytes:            input.ScratchBytes,
		VMMaxScratchBytes:       input.VMMaxScratchBytes,
		BuildCacheBytes:         input.BuildCacheBytes,
		ArtifactCacheBytes:      input.ArtifactCacheBytes,
		HugepagesBytes:          input.HugepagesBytes,
		CheckpointBytes:         input.CheckpointBytes,
		Observation:             input.Observation,
		Network: api.WorkerNetworkCapabilities{
			Internet:      input.Network.Internet,
			BlockInternet: input.Network.BlockInternet,
			DenyCIDRs:     input.Network.DenyCIDRs,
			AllowCIDRs:    input.Network.AllowCIDRs,
			AllowDomains:  input.Network.AllowDomains,
		},
	}
	labels, err := normalizeWorkerLabels(input.Labels)
	if err != nil {
		return api.WorkerCapabilities{}, err
	}
	capabilities.Labels = labels
	if capabilities.ProtocolVersion == "" {
		return api.WorkerCapabilities{}, errors.New("worker protocol_version is required")
	}
	if capabilities.ProtocolVersion != api.CurrentWorkerProtocolVersion {
		return api.WorkerCapabilities{}, fmt.Errorf("worker protocol_version %s is not supported; current protocol is %s", capabilities.ProtocolVersion, api.CurrentWorkerProtocolVersion)
	}
	if capabilities.RuntimeID == "" {
		return api.WorkerCapabilities{}, errors.New("worker runtime_id is required")
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
	if capabilities.InitramfsDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker initramfs_digest is required")
	}
	if capabilities.RootfsDigest == "" {
		return api.WorkerCapabilities{}, errors.New("worker rootfs_digest is required")
	}
	if capabilities.CNIProfile == "" {
		return api.WorkerCapabilities{}, errors.New("worker cni_profile is required")
	}
	expectedRuntimeID, err := compute.RuntimeIdentityDigest(compute.RuntimeSelector{
		Arch:            capabilities.RuntimeArch,
		ABI:             capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	})
	if err != nil {
		return api.WorkerCapabilities{}, fmt.Errorf("worker runtime identity: %w", err)
	}
	if capabilities.RuntimeID != expectedRuntimeID {
		return api.WorkerCapabilities{}, fmt.Errorf("worker runtime_id %s does not match runtime identity %s", capabilities.RuntimeID, expectedRuntimeID)
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
	if capabilities.VMMilliCPU <= 0 || capabilities.VMMilliCPU > capabilities.MaxVCPUs*1000 {
		return api.WorkerCapabilities{}, errors.New("worker vm_milli_cpu must be positive and not exceed aggregate CPU")
	}
	if capabilities.VMMemoryMiB <= 0 || capabilities.VMMemoryMiB > capabilities.MaxMemoryMiB {
		return api.WorkerCapabilities{}, errors.New("worker vm_memory_mib must be positive and not exceed aggregate memory")
	}
	if capabilities.MaxDiskMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_disk_mib must be positive")
	}
	if capabilities.MaxDiskMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_disk_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.VMMaxDiskMiB <= 0 || capabilities.VMMaxDiskMiB > capabilities.MaxDiskMiB {
		return api.WorkerCapabilities{}, errors.New("worker vm_max_disk_mib must be positive and not exceed aggregate max_disk_mib")
	}
	if capabilities.VMMaxScratchBytes <= 0 || capabilities.VMMaxScratchBytes > capabilities.ScratchBytes {
		return api.WorkerCapabilities{}, errors.New("worker vm_max_scratch_bytes must be positive and not exceed aggregate scratch_bytes")
	}
	if capabilities.ExecutionSlotsAvailable <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker execution_slots_available must be positive")
	}
	if !capabilities.SupportsRun && !capabilities.SupportsBuild {
		return api.WorkerCapabilities{}, errors.New("worker must support run, build, or both")
	}
	if capabilities.SupportsBuild && capabilities.MaxBuildExecutors <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_build_executors must be positive for build role")
	}
	if !capabilities.SupportsBuild && capabilities.MaxBuildExecutors != 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_build_executors must be zero without build role")
	}
	if capabilities.SupportsRun && capabilities.MaxRuntimeStarts <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_runtime_starts must be positive for run role")
	}
	if !capabilities.Network.Internet {
		return api.WorkerCapabilities{}, errors.New("worker network.internet capability is required")
	}
	if !capabilities.Network.BlockInternet {
		return api.WorkerCapabilities{}, errors.New("worker network.block_internet capability is required")
	}
	if !capabilities.Network.DenyCIDRs {
		return api.WorkerCapabilities{}, errors.New("worker network.deny_cidrs capability is required")
	}
	return capabilities, nil
}

func firstPositiveInt32(values ...int32) int32 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
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
