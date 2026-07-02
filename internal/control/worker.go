package control

import (
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
)

const defaultWorkerTokenTTL = 15 * time.Minute

func (s *Server) workerRegister(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("worker bootstrap storage is not configured")))
		return
	}
	if len(s.authSecret) == 0 {
		writeError(w, unavailable(errors.New("worker bootstrap is not configured")))
		return
	}
	var request api.WorkerRegisterRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker bootstrap request JSON: %w", err)))
		return
	}
	registrationHash, err := auth.HashToken(s.authSecret, request.BootstrapToken)
	if err != nil {
		writeError(w, unauthorized(errors.New("worker bootstrap token is required")))
		return
	}
	if strings.TrimSpace(request.BootstrapToken) == s.workerRegisterToken {
		if err := s.ensureWorkerBootstrapToken(r.Context(), s.db); err != nil {
			s.log.Error("worker bootstrap token bootstrap failed", "error", err)
			writeError(w, errors.New("configure worker bootstrap token"))
			return
		}
	}
	generated, err := auth.GenerateWorkerInstanceSecret(s.authSecret)
	if err != nil {
		writeError(w, errors.New("generate worker instance credential"))
		return
	}
	workerInstanceID := uuid.Must(uuid.NewV7())
	resourceID := strings.TrimSpace(request.ResourceID)
	if resourceID == "" {
		resourceID = workerInstanceID.String()
	}
	credential, err := s.db.CreateWorkerInstanceCredentialFromBootstrap(r.Context(), db.CreateWorkerInstanceCredentialFromBootstrapParams{
		BootstrapTokenHash: registrationHash,
		CellID:             s.cellID,
		CredentialID:       pgvalue.UUID(uuid.Must(uuid.NewV7())),
		WorkerInstanceID:   pgvalue.UUID(workerInstanceID),
		ResourceID:         resourceID,
		KeyPrefix:          generated.KeyPrefix,
		SecretHash:         generated.TokenHash,
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errors.New("worker bootstrap token is invalid")))
		return
	}
	if err != nil {
		s.log.Error("worker bootstrap failed", "worker_instance_id", workerInstanceID.String(), "resource_id", resourceID, "error", err)
		writeError(w, errors.New("register worker"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerRegisterResponse{
		WorkerInstanceID:     pgvalue.MustUUIDValue(credential.WorkerInstanceID).String(),
		WorkerGroupID:        pgvalue.MustUUIDValue(credential.WorkerGroupID).String(),
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
	credential, err := s.db.AuthenticateWorkerInstanceCredential(r.Context(), db.AuthenticateWorkerInstanceCredentialParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		SecretHash:       secretHash,
		CellID:           s.cellID,
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
	signed, err := auth.IssueWorkerToken(s.workerTokenSecret, auth.WorkerClaims{
		WorkerInstanceID: pgvalue.MustUUIDValue(credential.WorkerInstanceID).String(),
		CredentialID:     credentialID.String(),
		CellID:           credential.CellID,
		ClaimVersion:     credential.ClaimVersion,
		IssuedAt:         now,
		ExpiresAt:        expiresAt,
	})
	if err != nil {
		s.log.Error("mint worker token failed", "worker_instance_id", request.WorkerInstanceID, "error", err)
		writeError(w, errors.New("mint worker token"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerTokenResponse{
		Token:            signed,
		ExpiresInSeconds: int64(s.workerTokenTTL / time.Second),
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
	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker activate failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("activate worker"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, errors.New("select runtime release"))
		return
	}
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     pgvalue.UUID(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusActive,
	}); isNoRows(err) {
		writeError(w, notFound(errors.New("worker is not registered")))
		return
	} else if err != nil {
		s.log.Error("worker activate status failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("activate worker"))
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
	if _, err := s.db.SetWorkerInstanceStatus(r.Context(), db.SetWorkerInstanceStatusParams{
		ID:     pgvalue.UUID(worker.WorkerInstanceID),
		Status: db.WorkerInstanceStatusDraining,
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

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	s.writeWorkerStatus(w, r, workerFromContext(r.Context()))
}

func (s *Server) writeWorkerStatus(w http.ResponseWriter, r *http.Request, worker workerActor) {
	state, err := s.db.GetWorkerInstanceState(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
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
		WorkerGroupID:    pgvalue.MustUUIDValue(state.WorkerGroupID).String(),
		Status:           api.WorkerStatus(state.Status),
		ActiveExecutions: state.ActiveExecutions,
	})
}

func workerInstanceHeartbeatParams(worker workerActor, capabilities api.WorkerCapabilities) db.UpsertWorkerInstanceHeartbeatParams {
	resources := compute.ResourceVector{
		MilliCPU:  capabilities.MaxVCPUs * 1000,
		MemoryMiB: capabilities.MaxMemoryMiB,
		DiskMiB:   capabilities.MaxDiskMiB,
		Slots:     capabilities.ExecutionSlotsAvailable,
	}
	heartbeat, _ := json.Marshal(workerHeartbeatPayload{
		RuntimeID:       capabilities.RuntimeID,
		RuntimeArch:     capabilities.RuntimeArch,
		RuntimeABI:      capabilities.RuntimeABI,
		KernelDigest:    capabilities.KernelDigest,
		InitramfsDigest: capabilities.InitramfsDigest,
		RootfsDigest:    capabilities.RootfsDigest,
		CNIProfile:      capabilities.CNIProfile,
	})
	labels, _ := json.Marshal(capabilities.Labels)
	return db.UpsertWorkerInstanceHeartbeatParams{
		ID:                      pgvalue.UUID(worker.WorkerInstanceID),
		WorkerGroupID:           pgvalue.UUID(worker.WorkerGroupID),
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
		WorkerVersion:           capabilities.WorkerVersion,
		ProtocolVersion:         capabilities.ProtocolVersion,
		RuntimeID:               capabilities.RuntimeID,
		RuntimeArch:             capabilities.RuntimeArch,
		RuntimeABI:              capabilities.RuntimeABI,
		KernelDigest:            capabilities.KernelDigest,
		InitramfsDigest:         capabilities.InitramfsDigest,
		RootfsDigest:            capabilities.RootfsDigest,
		CniProfile:              capabilities.CNIProfile,
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
		MaxDiskMiB:              input.MaxDiskMiB,
		ExecutionSlotsAvailable: input.ExecutionSlotsAvailable,
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
	if capabilities.MaxDiskMiB <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker max_disk_mib must be positive")
	}
	if capabilities.MaxDiskMiB > math.MaxInt32 {
		return api.WorkerCapabilities{}, fmt.Errorf("worker max_disk_mib exceeds max %d", math.MaxInt32)
	}
	if capabilities.ExecutionSlotsAvailable <= 0 {
		return api.WorkerCapabilities{}, errors.New("worker execution_slots_available must be positive")
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
