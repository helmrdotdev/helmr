package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/vmplatform"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerEnroll(w http.ResponseWriter, r *http.Request) {
	if !s.workerEnrollmentGuard.allowEnrollment(workerEnrollmentSource(r), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeError(w, tooManyRequests(errors.New("worker enrollment rate limit exceeded")))
		return
	}
	limitRequestBodySize(w, r, 64<<10)
	var request workerapi.EnrollmentRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker enrollment JSON: %w", err))
		return
	}
	if err := checkWorkerAPIVersion(request.APIVersion); err != nil {
		writeError(w, err)
		return
	}
	// An unparsable enrollment token is rejected by the owner after it has
	// validated the rest of the request.
	tokenHash, err := strictWorkerEnrollmentBearer(r.Header.Values("Authorization"))
	if err != nil {
		tokenHash = nil
	}
	enrolled, err := workergroup.EnrollHost(r.Context(), s.db, s.hostAuth, workergroup.Enrollment{
		TokenHash: tokenHash, PoolName: request.PoolName, ResourceID: request.ResourceID,
	})
	if err != nil {
		s.writeWorkerHostError(w, "enroll worker", err)
		return
	}
	writeJSON(w, http.StatusCreated, workerapi.EnrollmentResponse{
		WorkerHostID:     enrolled.HostID.String(),
		WorkerGroupID:    enrolled.GroupID.String(),
		WorkerPoolID:     enrolled.PoolID.String(),
		WorkerHostSecret: enrolled.Secret,
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
	return auth.ParseEnrollmentToken(raw)
}

func (s *Server) workerIssueHostCredential(w http.ResponseWriter, r *http.Request) {
	var request workerapi.HostCredentialRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker host credential request JSON: %w", err))
		return
	}
	if err := checkWorkerAPIVersion(request.APIVersion); err != nil {
		writeError(w, err)
		return
	}
	credential, err := workergroup.IssueHostCredential(r.Context(), s.db, s.hostAuth, workergroup.HostCredentialRequest{
		HostID: request.WorkerHostID, Secret: request.WorkerHostSecret, ServiceID: request.ServiceID,
	}, time.Now)
	if err != nil {
		s.writeWorkerHostError(w, "issue worker host credential", err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.HostCredentialResponse{
		Credential:       credential.Value,
		ExpiresInSeconds: int64(credential.ExpiresIn / time.Second),
		WorkerEpoch:      credential.Epoch,
	})
}

func (s *Server) workerActivate(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ActivateRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker activate request JSON: %w", err))
		return
	}
	if err := checkWorkerAPIVersion(request.APIVersion); err != nil {
		writeError(w, err)
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	cpuEnvironment, err := json.Marshal(capabilities.CPUEnvironment)
	if err != nil {
		s.writeWorkerHostError(w, "activate worker", fmt.Errorf("encode Worker CPU environment: %w", err))
		return
	}
	if err := workergroup.ActivateHost(r.Context(), s.tx, worker, workergroup.Activation{
		Template:             workerTemplate(capabilities),
		CPUEnvironment:       cpuEnvironment,
		CPUEnvironmentDigest: capabilities.CPUEnvironment.Digest,
	}); err != nil {
		s.writeWorkerHostError(w, "activate worker", err)
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerStartupRecovery(w http.ResponseWriter, r *http.Request) {
	var request workerapi.StartupRecoveryRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker startup recovery JSON: %w", err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := validateWorkerStartupRecovery(request); err != nil {
		writeError(w, badRequest(err))
		return
	}
	evidence, err := json.Marshal(request)
	if err != nil {
		writeError(w, badRequest(errors.New("encode startup recovery evidence")))
		return
	}
	if err := workergroup.RecordStartupRecovery(r.Context(), s.db, worker, evidence); err != nil {
		s.writeWorkerHostError(w, "record worker startup recovery", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func validateWorkerStartupRecovery(request workerapi.StartupRecoveryRequest) error {
	if request.Quarantined == nil {
		return errors.New("quarantined must be an array")
	}
	seen := make(map[uuid.UUID]struct{}, len(request.Quarantined))
	for _, value := range request.Quarantined {
		id, err := ids.Parse(value)
		if err != nil {
			return errors.New("quarantined instance id must be a canonical UUIDv7")
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("quarantined instance id %s is duplicated", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

func (s *Server) workerObserve(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ObserveRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker observation JSON: %w", err))
		return
	}
	worker := workerFromContext(r.Context())
	if err := workergroup.RecordObservation(r.Context(), s.db, worker, workergroup.HostObservation{
		RunPausedReason: request.Observation.RunPausedReason,
		VMPausedReason:  request.Observation.VMPausedReason,
	}); err != nil {
		s.writeWorkerHostError(w, "record worker observation", err)
		return
	}
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerDrain(w http.ResponseWriter, r *http.Request) {
	worker := workerFromContext(r.Context())
	if err := workergroup.BeginHostDrain(r.Context(), s.tx, worker); err != nil {
		s.writeWorkerHostError(w, "drain worker", err)
		return
	}
	s.captureDrainingComputers(r.Context(), worker.HostID, uuid.Nil())
	s.writeWorkerStatus(w, r, worker)
}

func (s *Server) workerCompleteDrain(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength != 0 {
		writeError(w, badRequest(errors.New("drain completion must have no request body")))
		return
	}
	worker := workerFromContext(r.Context())
	completed, err := workergroup.CompleteHostDrain(r.Context(), s.tx, worker)
	if err != nil {
		s.writeWorkerHostError(w, "complete worker drain", err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StatusResponse{
		WorkerHostID:    pgvalue.MustUUIDValue(completed.ID).String(),
		WorkerGroupID:   pgvalue.UUIDString(completed.WorkerGroupID),
		Status:          workerapi.StatusTerminationReady,
		ActiveInstances: 0,
	})
}

func (s *Server) workerFence(w http.ResponseWriter, r *http.Request) {
	var request workerapi.FenceRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker fence request JSON: %w", err))
		return
	}
	if err := workergroup.FenceHost(r.Context(), s.db, workerFromContext(r.Context()), request.ReasonCode); err != nil {
		s.writeWorkerHostError(w, "fence worker", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) workerStatus(w http.ResponseWriter, r *http.Request) {
	s.writeWorkerStatus(w, r, workerFromContext(r.Context()))
}

func (s *Server) writeWorkerStatus(w http.ResponseWriter, r *http.Request, worker workergroup.HostPrincipal) {
	state, err := workergroup.ReadHost(r.Context(), s.db, worker)
	if err != nil {
		s.writeWorkerHostError(w, "get worker status", err)
		return
	}
	readiness := workerapi.Readiness{
		Run:      workerRoleReadiness(state, state.RunReady, state.RunPausedReason),
		Instance: workerRoleReadiness(state, state.InstanceReady, state.VMPausedReason),
	}
	status, err := workerPublicStatus(state.Status)
	if err != nil {
		s.log.Error("project worker status", "worker_host_id", worker.HostID.String(), "error", err)
		writeError(w, errors.New("project worker status"))
		return
	}
	writeJSON(w, http.StatusOK, workerapi.StatusResponse{
		WorkerHostID:    pgvalue.MustUUIDValue(state.ID).String(),
		WorkerGroupID:   pgvalue.UUIDString(state.WorkerGroupID),
		Status:          status,
		ActiveInstances: state.ActiveInstances,
		Readiness:       readiness,
	})
}

// writeWorkerHostError writes a worker host lifecycle error; a failure the
// client is not told about is logged and reported as the operation.
func (s *Server) writeWorkerHostError(w http.ResponseWriter, operation string, err error) {
	mapped := workerGroupError(err)
	if errorStatus(mapped) != http.StatusInternalServerError {
		writeError(w, mapped)
		return
	}
	s.log.Error("worker host request failed", "operation", operation, "error", err)
	writeError(w, errors.New(operation))
}

func workerPublicStatus(state string) (workerapi.Status, error) {
	switch state {
	case db.WorkerHostStatusActive:
		return workerapi.StatusActive, nil
	case db.WorkerHostStatusDraining:
		return workerapi.StatusDraining, nil
	case db.WorkerHostStatusTerminationReady:
		return workerapi.StatusTerminationReady, nil
	default:
		return "", fmt.Errorf("worker instance state %q has no Worker projection", state)
	}
}

func workerRoleReadiness(
	state db.GetWorkerHostStatusRow,
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
	case state.Status != string(db.WorkerHostStatusActive):
		result.PausedReason = "worker_not_active"
	case !state.ObservedAt.Valid:
		result.PausedReason = "observation_missing"
	default:
		result.PausedReason = "observation_stale"
	}
	return result
}

func workerTemplate(capabilities workerapi.Capabilities) workergroup.Template {
	return workergroup.Template{
		Schema: workergroup.TemplateSchema, Runtime: capabilities.Runtime,
		CPUShapes: append([]vmplatform.CPUShape(nil), capabilities.CPUShapes...),
		Capacity: workergroup.ResourceVector{
			CPUMillis: capabilities.MaxVCPUs * 1000, MemoryBytes: capabilities.MaxMemoryMiB * 1024 * 1024,
			GuestEphemeralDiskBytes: capabilities.GuestEphemeralDiskBytes,
			VMSlots:                 int64(capabilities.ExecutionSlotsAvailable),
		},
		PerVM: workergroup.ResourceVector{
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
	cpuShapes := append([]vmplatform.CPUShape(nil), input.CPUShapes...)
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
	template := workerTemplate(capabilities)
	if err := template.Validate(); err != nil {
		return workerapi.Capabilities{}, fmt.Errorf("worker template: %w", err)
	}
	return capabilities, nil
}
