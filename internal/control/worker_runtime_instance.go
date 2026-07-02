package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"net/http"
	"strings"
	"time"
)

func (s *Server) workerCreatePreparedRuntimeInstance(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerPreparedRuntimeInstanceCreateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker prepared runtime instance request JSON: %w", err)))
		return
	}
	id, err := parseWorkspaceUUID("id", request.ID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	workspaceMountID, err := parseWorkspaceUUID("workspace_mount_id", request.WorkspaceMountID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if strings.TrimSpace(request.RuntimeKeyHash) == "" {
		writeError(w, badRequest(errors.New("runtime_key_hash is required")))
		return
	}
	if strings.TrimSpace(request.InstanceToken) == "" {
		writeError(w, badRequest(errors.New("instance_token is required")))
		return
	}
	if request.ExpiresAt.IsZero() {
		writeError(w, badRequest(errors.New("expires_at is required")))
		return
	}
	if strings.TrimSpace(request.GuestdChannelToken) == "" {
		writeError(w, badRequest(errors.New("guestd_channel_token is required")))
		return
	}
	worker := workerFromContext(r.Context())
	runtimeReleaseID, ok := s.workerRuntimeReleaseID(w, r, worker)
	if !ok {
		return
	}
	row, err := s.db.CreatePreparedRuntimeInstanceForWorkspaceMountSource(r.Context(), db.CreatePreparedRuntimeInstanceForWorkspaceMountSourceParams{
		ID:                     id,
		WorkspaceMountID:       workspaceMountID,
		WorkerInstanceID:       pgvalue.UUID(worker.WorkerInstanceID),
		RuntimeReleaseID:       runtimeReleaseID,
		GuestdChannelTokenHash: guestdChannelTokenHash(request.GuestdChannelToken),
		RuntimeKeyHash:         strings.TrimSpace(request.RuntimeKeyHash),
		RuntimeKey:             normalizedJSONRawMessage(request.RuntimeKey),
		NetworkPolicy:          normalizedJSONRawMessage(request.NetworkPolicy),
		InstanceToken:          strings.TrimSpace(request.InstanceToken),
		ExpiresAt:              pgvalue.Timestamptz(request.ExpiresAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(codedError{code: "capacity_unavailable", message: "worker capacity is not available for a prepared runtime instance"}))
		return
	}
	if err != nil {
		s.log.Error("create prepared runtime instance failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("create prepared runtime instance"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerPreparedRuntimeInstanceCreateResponse{Instance: runtimeInstanceResponse(row)})
}

func (s *Server) workerCreateRuntimePrepareInstance(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimePrepareInstanceCreateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker prepared runtime warm instance request JSON: %w", err)))
		return
	}
	id, err := parseWorkspaceUUID("id", request.ID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	deploymentSandboxID, err := parseWorkspaceUUID("deployment_sandbox_id", request.DeploymentSandboxID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if strings.TrimSpace(request.RuntimeID) == "" {
		writeError(w, badRequest(errors.New("runtime_id is required")))
		return
	}
	if strings.TrimSpace(request.RootfsDigest) == "" {
		writeError(w, badRequest(errors.New("rootfs_digest is required")))
		return
	}
	if strings.TrimSpace(request.RuntimeABI) == "" {
		writeError(w, badRequest(errors.New("runtime_abi is required")))
		return
	}
	if strings.TrimSpace(request.RuntimeKeyHash) == "" {
		writeError(w, badRequest(errors.New("runtime_key_hash is required")))
		return
	}
	if strings.TrimSpace(request.InstanceToken) == "" {
		writeError(w, badRequest(errors.New("instance_token is required")))
		return
	}
	if request.ExpiresAt.IsZero() {
		writeError(w, badRequest(errors.New("expires_at is required")))
		return
	}
	worker := workerFromContext(r.Context())
	runtimeReleaseID := strings.TrimSpace(request.RuntimeID)
	row, err := s.db.CreateRuntimeInstanceForDeploymentSandbox(r.Context(), db.CreateRuntimeInstanceForDeploymentSandboxParams{
		ID:                  id,
		DeploymentSandboxID: deploymentSandboxID,
		WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID),
		RuntimeReleaseID:    runtimeReleaseID,
		RootfsDigest:        strings.TrimSpace(request.RootfsDigest),
		RuntimeABI:          strings.TrimSpace(request.RuntimeABI),
		RuntimeKeyHash:      strings.TrimSpace(request.RuntimeKeyHash),
		RuntimeKey:          normalizedJSONRawMessage(request.RuntimeKey),
		InstanceToken:       strings.TrimSpace(request.InstanceToken),
		ExpiresAt:           pgvalue.Timestamptz(request.ExpiresAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, conflict(codedError{code: "capacity_unavailable", message: "worker capacity is not available for a prepared runtime warm instance"}))
		return
	}
	if err != nil {
		s.log.Error("create prepared runtime warm instance failed", "worker_instance_id", worker.WorkerInstanceID.String(), "deployment_sandbox_id", request.DeploymentSandboxID, "error", err)
		writeError(w, errors.New("create prepared runtime warm instance"))
		return
	}
	writeJSON(w, http.StatusCreated, api.WorkerRuntimePrepareInstanceCreateResponse{
		Instance: runtimeInstanceResponse(runtimeInstanceFromDeploymentSandboxRow(row)),
		Source:   preparedRuntimeSourceResponse(row),
	})
}

func (s *Server) workerRenewRuntimeInstance(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRuntimeInstanceRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime instance renew request JSON: %w", err)))
		return
	}
	id, err := parseWorkspaceUUID("id", request.ID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if strings.TrimSpace(request.InstanceToken) == "" {
		writeError(w, badRequest(errors.New("instance_token is required")))
		return
	}
	if request.ExpiresAt.IsZero() {
		writeError(w, badRequest(errors.New("expires_at is required")))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.RenewRuntimeInstance(r.Context(), db.RenewRuntimeInstanceParams{
		ID:               id,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		InstanceToken:    strings.TrimSpace(request.InstanceToken),
		ExpiresAt:        pgvalue.Timestamptz(request.ExpiresAt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("runtime instance not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew runtime instance"))
		return
	}
	writeJSON(w, http.StatusOK, runtimeInstanceResponse(row))
}

func (s *Server) workerMarkRuntimeInstanceReady(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "ready")
}

func (s *Server) workerMarkRuntimeInstanceClosed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "closed")
}

func (s *Server) workerMarkRuntimeInstanceFailed(w http.ResponseWriter, r *http.Request) {
	s.workerMarkRuntimeInstance(w, r, "failed")
}

func (s *Server) workerMarkRuntimeInstance(w http.ResponseWriter, r *http.Request, state string) {
	var request api.WorkerRuntimeInstanceStateRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime instance %s request JSON: %w", state, err)))
		return
	}
	id, err := parseWorkspaceUUID("id", request.ID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if strings.TrimSpace(request.InstanceToken) == "" {
		writeError(w, badRequest(errors.New("instance_token is required")))
		return
	}
	worker := workerFromContext(r.Context())
	var row db.RuntimeInstance
	switch state {
	case "ready":
		if request.ExpiresAt.IsZero() {
			writeError(w, badRequest(errors.New("expires_at is required")))
			return
		}
		runtimeSubstrateArtifactID := pgtype.UUID{}
		if strings.TrimSpace(request.RuntimeSubstrateArtifactID) != "" {
			runtimeSubstrateArtifactID, err = parseWorkspaceUUID("runtime_substrate_artifact_id", request.RuntimeSubstrateArtifactID)
			if err != nil {
				writeError(w, badRequest(err))
				return
			}
		}
		row, err = s.db.MarkRuntimeInstanceReady(r.Context(), db.MarkRuntimeInstanceReadyParams{
			ID:                         id,
			WorkerInstanceID:           pgvalue.UUID(worker.WorkerInstanceID),
			InstanceToken:              strings.TrimSpace(request.InstanceToken),
			RuntimeSubstrateArtifactID: runtimeSubstrateArtifactID,
			ExpiresAt:                  pgvalue.Timestamptz(request.ExpiresAt),
		})
	case "closed":
		closed, closedErr := s.db.MarkRuntimeInstanceClosed(r.Context(), db.MarkRuntimeInstanceClosedParams{
			ID:               id,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			InstanceToken:    strings.TrimSpace(request.InstanceToken),
		})
		err = closedErr
		row = db.RuntimeInstance(closed)
	case "failed":
		row, err = s.db.MarkRuntimeInstanceFailed(r.Context(), db.MarkRuntimeInstanceFailedParams{
			ID:               id,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			InstanceToken:    strings.TrimSpace(request.InstanceToken),
			Error:            normalizedJSONRawMessage(request.Error),
		})
	default:
		writeError(w, errors.New("unsupported runtime instance state"))
		return
	}
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("runtime instance not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark runtime instance "+state))
		return
	}
	writeJSON(w, http.StatusOK, runtimeInstanceResponse(row))
}

func normalizedJSONRawMessage(raw json.RawMessage) []byte {
	if strings.TrimSpace(string(raw)) == "" {
		return []byte(`{}`)
	}
	return []byte(raw)
}

func (s *Server) workerRuntimeReleaseID(w http.ResponseWriter, r *http.Request, worker workerActor) (string, bool) {
	state, err := s.db.GetWorkerInstanceState(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("worker instance not found")))
		return "", false
	}
	if err != nil {
		writeError(w, errors.New("load worker runtime release"))
		return "", false
	}
	return state.RuntimeID, true
}

func runtimeInstanceResponse(row db.RuntimeInstance) api.WorkerRuntimeInstance {
	return api.WorkerRuntimeInstance{
		ID:                     pgvalue.UUIDString(row.ID),
		OrgID:                  pgvalue.UUIDString(row.OrgID),
		ProjectID:              pgvalue.UUIDString(row.ProjectID),
		EnvironmentID:          pgvalue.UUIDString(row.EnvironmentID),
		WorkerInstanceID:       pgvalue.UUIDString(row.WorkerInstanceID),
		RuntimeEpoch:           row.RuntimeEpoch,
		RuntimeKeyHash:         row.RuntimeKeyHash,
		RuntimeKey:             json.RawMessage(row.RuntimeKey),
		RuntimeID:              row.RuntimeReleaseID,
		DeploymentSandboxID:    pgvalue.UUIDString(row.DeploymentSandboxID),
		State:                  string(row.State),
		InstanceToken:          row.InstanceToken,
		ReservedCpuMillis:      row.ReservedCpuMillis,
		ReservedMemoryMiB:      row.ReservedMemoryMib,
		ReservedDiskMiB:        row.ReservedDiskMib,
		ReservedExecutionSlots: row.ReservedExecutionSlots,
		WorkspaceMountID:       pgvalue.UUIDString(row.WorkspaceMountID),
		ExpiresAt:              controlOptionalTimestamptz(row.ExpiresAt),
	}
}

func runtimeInstanceFromDeploymentSandboxRow(row db.CreateRuntimeInstanceForDeploymentSandboxRow) db.RuntimeInstance {
	return db.RuntimeInstance{
		ID:                         row.ID,
		OrgID:                      row.OrgID,
		ProjectID:                  row.ProjectID,
		EnvironmentID:              row.EnvironmentID,
		WorkerInstanceID:           row.WorkerInstanceID,
		RuntimeReleaseID:           row.RuntimeReleaseID,
		DeploymentSandboxID:        row.DeploymentSandboxID,
		RuntimeSubstrateArtifactID: row.RuntimeSubstrateArtifactID,
		RuntimeEpoch:               row.RuntimeEpoch,
		RuntimeKeyHash:             row.RuntimeKeyHash,
		RuntimeKey:                 row.RuntimeKey,
		SandboxFingerprint:         row.SandboxFingerprint,
		RootfsDigest:               row.RootfsDigest,
		ImageDigest:                row.ImageDigest,
		ImageFormat:                row.ImageFormat,
		SandboxImageArtifactID:     row.SandboxImageArtifactID,
		SandboxImageArtifactDigest: row.SandboxImageArtifactDigest,
		SandboxImageArtifactFormat: row.SandboxImageArtifactFormat,
		WorkspaceMountPath:         row.WorkspaceMountPath,
		RuntimeABI:                 row.RuntimeABI,
		GuestdAbi:                  row.GuestdAbi,
		AdapterAbi:                 row.AdapterAbi,
		NetworkPolicy:              row.NetworkPolicy,
		ReservedCpuMillis:          row.ReservedCpuMillis,
		ReservedMemoryMib:          row.ReservedMemoryMib,
		ReservedDiskMib:            row.ReservedDiskMib,
		ReservedExecutionSlots:     row.ReservedExecutionSlots,
		WorkspaceMountID:           row.WorkspaceMountID,
		State:                      row.State,
		InstanceToken:              row.InstanceToken,
		LastHeartbeatAt:            row.LastHeartbeatAt,
		ExpiresAt:                  row.ExpiresAt,
		PreparedAt:                 row.PreparedAt,
		BoundAt:                    row.BoundAt,
		RunningAt:                  row.RunningAt,
		ClosedAt:                   row.ClosedAt,
		LostAt:                     row.LostAt,
		FailedAt:                   row.FailedAt,
		Error:                      row.Error,
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
	}
}

func preparedRuntimeSourceResponse(row db.CreateRuntimeInstanceForDeploymentSandboxRow) api.WorkerPreparedRuntimeSource {
	return api.WorkerPreparedRuntimeSource{
		DeploymentSandboxID:        pgvalue.UUIDString(row.DeploymentSandboxID),
		RuntimeID:                  row.RuntimeReleaseID,
		SandboxImageArtifact:       api.CASObject{Digest: row.SandboxImageArtifactDigest, SizeBytes: row.SandboxImageArtifactSizeBytes, MediaType: row.SandboxImageArtifactMediaType},
		SandboxImageArtifactFormat: row.SandboxImageArtifactFormat,
		RootfsDigest:               row.RootfsDigest,
		ImageDigest:                row.ImageDigest,
		ImageFormat:                row.ImageFormat,
		WorkspaceMountPath:         row.WorkspaceMountPath,
		ReservedCpuMillis:          row.ReservedCpuMillis,
		ReservedMemoryMiB:          row.ReservedMemoryMib,
		ReservedDiskMiB:            row.ReservedDiskMib,
		ReservedExecutionSlots:     row.ReservedExecutionSlots,
		RuntimeABI:                 row.RuntimeABI,
		GuestdABI:                  row.GuestdAbi,
		AdapterABI:                 row.AdapterAbi,
	}
}

func controlOptionalTimestamptz(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	return &value.Time
}
