package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/runtime"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

func (s *Server) workerClaimWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountClaimRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount claim request JSON: %w", err)))
		return
	}
	capabilities, err := normalizeWorkerCapabilities(request.Capabilities)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if _, err := s.db.UpsertWorkerInstanceHeartbeat(r.Context(), workerInstanceHeartbeatParams(worker, capabilities)); err != nil {
		s.log.Error("worker heartbeat failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("record worker heartbeat"))
		return
	}
	if err := s.db.EnsureRuntimeReleaseSelection(r.Context(), capabilities.RuntimeID); err != nil {
		s.log.Error("ensure runtime release selection failed", "worker_instance_id", worker.WorkerInstanceID.String(), "runtime_id", capabilities.RuntimeID, "error", err)
		writeError(w, errors.New("select runtime release"))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	if err := s.releaseExpiredPreparedRuntimeReservations(r.Context()); err != nil {
		s.log.Error("release expired prepared runtime reservations failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("release expired prepared runtime reservations"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
	if isNoRows(err) {
		s.requestCapacityPressureIdleWorkspaceStops(r.Context(), worker.WorkerInstanceID, "worker_capacity_missing")
		s.createCapacityPressureLiveRuntimeCheckpointWaitCommands(r.Context(), worker.WorkerInstanceID, "worker_capacity_missing")
		if s.log != nil {
			s.log.Info("worker workspace mount claim skipped",
				"worker_instance_id", worker.WorkerInstanceID.String(),
				"reason", "worker_capacity_missing",
			)
		}
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 || capacity.AvailableDiskMib <= 0 {
		s.requestCapacityPressureIdleWorkspaceStops(r.Context(), worker.WorkerInstanceID, "no_available_capacity")
		s.createCapacityPressureLiveRuntimeCheckpointWaitCommands(r.Context(), worker.WorkerInstanceID, "no_available_capacity")
		if s.log != nil {
			s.log.Info("worker workspace mount claim capacity constrained",
				"worker_instance_id", worker.WorkerInstanceID.String(),
				"reason", "no_available_capacity",
				"available_execution_slots", capacity.AvailableExecutionSlots,
				"available_milli_cpu", capacity.AvailableMilliCpu,
				"available_memory_mib", capacity.AvailableMemoryMib,
				"available_disk_mib", capacity.AvailableDiskMib,
			)
		}
	}
	guestdChannelToken, err := newGuestdChannelToken()
	if err != nil {
		writeError(w, errors.New("generate workspace mount guest channel token"))
		return
	}
	runtimeInstanceToken, err := runtime.NewInstanceToken()
	if err != nil {
		writeError(w, errors.New("generate workspace mount runtime instance token"))
		return
	}
	networkPolicy, err := json.Marshal(compute.DefaultNetworkPolicy())
	if err != nil {
		writeError(w, errors.New("encode workspace mount runtime network policy"))
		return
	}
	row, err := s.db.ClaimWorkspaceMount(r.Context(), db.ClaimWorkspaceMountParams{
		RootfsDigest:                capabilities.RootfsDigest,
		RuntimeABI:                  capabilities.RuntimeABI,
		GuestdAbi:                   currentGuestdABI,
		AdapterAbi:                  currentAdapterABI,
		NetworkPolicy:               networkPolicy,
		RuntimeInstanceID:           pgvalue.UUID(uuid.Must(uuid.NewV7())),
		RuntimeInstanceToken:        runtimeInstanceToken,
		WorkerInstanceID:            pgvalue.UUID(worker.WorkerInstanceID),
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(workspaceMountReservationDuration), Valid: true},
		GuestdChannelTokenHash:      guestdChannelTokenHash(guestdChannelToken),
		RuntimeID:                   capabilities.RuntimeID,
	})
	if isNoRows(err) {
		reserved, reserveErr := s.db.ReserveWorkspaceMountPreparingRuntime(r.Context(), db.ReserveWorkspaceMountPreparingRuntimeParams{
			RootfsDigest:                capabilities.RootfsDigest,
			RuntimeABI:                  capabilities.RuntimeABI,
			GuestdAbi:                   currentGuestdABI,
			AdapterAbi:                  currentAdapterABI,
			WorkerInstanceID:            pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeID:                   capabilities.RuntimeID,
			GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(preparedRuntimeReservationDuration), Valid: true},
		})
		if reserveErr == nil {
			if s.log != nil {
				s.log.Info("worker workspace mount awaiting prepared runtime",
					"worker_instance_id", worker.WorkerInstanceID.String(),
					"workspace_mount_id", pgvalue.UUIDString(reserved.ID),
					"preparing_runtime_instance_id", pgvalue.UUIDString(reserved.PreparingRuntimeInstanceID),
					"runtime_id", capabilities.RuntimeID,
					"rootfs_digest", capabilities.RootfsDigest,
					"runtime_abi", capabilities.RuntimeABI,
				)
			}
			writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{})
			return
		}
		if !isNoRows(reserveErr) {
			s.log.Error("reserve workspace mount preparing runtime failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", reserveErr)
			writeError(w, errors.New("reserve workspace mount preparing runtime"))
			return
		}
		awaiting, awaitingErr := s.db.GetAwaitingPreparedRuntimeMountForWorker(r.Context(), db.GetAwaitingPreparedRuntimeMountForWorkerParams{
			RootfsDigest:     capabilities.RootfsDigest,
			RuntimeABI:       capabilities.RuntimeABI,
			GuestdAbi:        currentGuestdABI,
			AdapterAbi:       currentAdapterABI,
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeID:        capabilities.RuntimeID,
		})
		if awaitingErr == nil {
			if s.log != nil {
				s.log.Info("worker workspace mount still awaiting prepared runtime",
					"worker_instance_id", worker.WorkerInstanceID.String(),
					"workspace_mount_id", pgvalue.UUIDString(awaiting.ID),
					"preparing_runtime_instance_id", pgvalue.UUIDString(awaiting.PreparingRuntimeInstanceID),
					"runtime_id", capabilities.RuntimeID,
				)
			}
			writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{})
			return
		}
		if !isNoRows(awaitingErr) {
			s.log.Error("get awaiting prepared runtime mount failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", awaitingErr)
			writeError(w, errors.New("get awaiting prepared runtime mount"))
			return
		}
		s.requestCapacityPressureIdleWorkspaceStops(r.Context(), worker.WorkerInstanceID, "no_claimable_mount")
		s.createCapacityPressureLiveRuntimeCheckpointWaitCommands(r.Context(), worker.WorkerInstanceID, "no_claimable_mount")
		if s.log != nil {
			s.log.Info("worker workspace mount claim skipped",
				"worker_instance_id", worker.WorkerInstanceID.String(),
				"reason", "no_claimable_mount",
				"available_execution_slots", capacity.AvailableExecutionSlots,
				"available_milli_cpu", capacity.AvailableMilliCpu,
				"available_memory_mib", capacity.AvailableMemoryMib,
				"available_disk_mib", capacity.AvailableDiskMib,
				"runtime_id", capabilities.RuntimeID,
				"rootfs_digest", capabilities.RootfsDigest,
				"runtime_abi", capabilities.RuntimeABI,
			)
		}
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("claim workspace mount failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("claim workspace mount"))
		return
	}
	mount := workerWorkspaceMountFromClaim(row)
	mount.GuestdChannelToken = guestdChannelToken
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountClaimResponse{Mount: mount})
}

func (s *Server) workerRenewWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount renew request JSON: %w", err)))
		return
	}
	row, err := s.workerRenewWorkspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew workspace mount"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(row))
}

func (s *Server) workerMarkWorkspaceMountMounted(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountMountedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount mounted request JSON: %w", err)))
		return
	}
	row, err := s.workerMarkWorkspaceMountMountedTransition(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark workspace mount mounted"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMountResponse(row))
}

func (s *Server) workerCaptureWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountCaptureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount capture request JSON: %w", err)))
		return
	}
	params, err := workerWorkspaceMountTransitionParams(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if err != nil {
		writeError(w, err)
		return
	}
	projectID, err := uuid.Parse(strings.TrimSpace(request.ProjectID))
	if err != nil {
		writeError(w, badRequest(errors.New("project_id must be a UUID")))
		return
	}
	environmentID, err := uuid.Parse(strings.TrimSpace(request.EnvironmentID))
	if err != nil {
		writeError(w, badRequest(errors.New("environment_id must be a UUID")))
		return
	}
	workspaceID, err := uuid.Parse(strings.TrimSpace(request.WorkspaceID))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_id must be a UUID")))
		return
	}
	digest := strings.TrimSpace(request.ArtifactDigest)
	if digest == "" {
		writeError(w, badRequest(errors.New("artifact_digest is required")))
		return
	}
	if request.ArtifactSizeBytes <= 0 {
		writeError(w, badRequest(errors.New("artifact_size_bytes must be positive")))
		return
	}
	if strings.TrimSpace(request.ArtifactMediaType) != workspace.ArtifactMediaType {
		writeError(w, badRequest(errors.New("artifact_media_type is unsupported")))
		return
	}
	if strings.TrimSpace(request.ArtifactEncoding) != workspace.ArtifactEncoding {
		writeError(w, badRequest(errors.New("artifact_encoding is unsupported")))
		return
	}
	if request.ArtifactEntryCount < 0 {
		writeError(w, badRequest(errors.New("artifact_entry_count must be non-negative")))
		return
	}
	var response api.WorkerWorkspaceMountCaptureResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID:     params.OrgID,
			CellID:    params.CellID,
			Digest:    digest,
			SizeBytes: request.ArtifactSizeBytes,
			MediaType: strings.TrimSpace(request.ArtifactMediaType),
		}); err != nil {
			return errors.New("record workspace capture CAS object")
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     params.OrgID,
			CellID:                    params.CellID,
			ProjectID:                 pgvalue.UUID(projectID),
			EnvironmentID:             pgvalue.UUID(environmentID),
			Digest:                    digest,
			Kind:                      db.ArtifactKindWorkspaceVersion,
			SizeBytes:                 request.ArtifactSizeBytes,
			MediaType:                 strings.TrimSpace(request.ArtifactMediaType),
			CreatedByWorkerInstanceID: params.WorkerInstanceID,
		})
		if err != nil {
			return errors.New("record workspace capture artifact")
		}
		version, err := work.q.PromoteWorkspaceMountStopCapture(r.Context(), db.PromoteWorkspaceMountStopCaptureParams{
			OrgID:                params.OrgID,
			ID:                   params.ID,
			WorkspaceID:          pgvalue.UUID(workspaceID),
			WorkerInstanceID:     params.WorkerInstanceID,
			RuntimeInstanceToken: params.RuntimeInstanceToken,
			ProjectID:            pgvalue.UUID(projectID),
			EnvironmentID:        pgvalue.UUID(environmentID),
			ArtifactID:           artifact.ID,
			SizeBytes:            request.ArtifactSizeBytes,
			ArtifactEncoding:     strings.TrimSpace(request.ArtifactEncoding),
			ContentDigest:        digest,
			VersionID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
			ArtifactEntryCount:   request.ArtifactEntryCount,
			Message:              "system capture before workspace stop",
		})
		if isNoRows(err) {
			return conflict(codedError{code: "workspace_mount_capture_rejected", message: "workspace mount capture is stale"})
		}
		if err != nil {
			return errors.New("promote workspace mount capture")
		}
		response = api.WorkerWorkspaceMountCaptureResponse{
			VersionID: pgvalue.MustUUIDValue(version.ID).String(),
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) workerFailWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountFailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount fail request JSON: %w", err)))
		return
	}
	errorJSON, err := normalizedJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params, err := workerWorkspaceMountTransitionParams(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if err != nil {
		writeError(w, err)
		return
	}
	var response api.WorkspaceMountResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		row, err := work.q.FailWorkspaceMount(r.Context(), db.FailWorkspaceMountParams{
			OrgID:                params.OrgID,
			ID:                   params.ID,
			WorkerInstanceID:     params.WorkerInstanceID,
			RuntimeInstanceToken: params.RuntimeInstanceToken,
			Error:                errorJSON,
		})
		if isNoRows(err) {
			return conflict(errors.New("workspace mount is stale"))
		}
		if err != nil {
			return errors.New("fail workspace mount")
		}
		if err := failQueuedRunsForWorkspaceMountFailure(r.Context(), work.q, row, errorJSON); err != nil {
			return errors.New("fail queued runs waiting for workspace mount")
		}
		response = failedWorkspaceMountResponse(row)
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func newGuestdChannelToken() (string, error) {
	return token.GenerateOpaque(32)
}

func guestdChannelTokenHash(token string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(token)))
}

func failedWorkspaceMountResponse(row db.FailWorkspaceMountRow) api.WorkspaceMountResponse {
	return workspaceMountResponse(db.WorkspaceMount{
		ID:                  row.ID,
		ProjectID:           row.ProjectID,
		EnvironmentID:       row.EnvironmentID,
		WorkspaceID:         row.WorkspaceID,
		DeploymentSandboxID: row.DeploymentSandboxID,
		BaseVersionID:       row.BaseVersionID,
		RuntimeInstanceID:   row.RuntimeInstanceID,
		State:               row.State,
		ClaimAttempt:        row.ClaimAttempt,
		FencingGeneration:   row.FencingGeneration,
		DirtyGeneration:     row.DirtyGeneration,
		LastHeartbeatAt:     row.LastHeartbeatAt,
		CreatedAt:           row.CreatedAt,
		UpdatedAt:           row.UpdatedAt,
	})
}

type workerWorkspaceMountFields struct {
	id                         pgtype.UUID
	orgID                      pgtype.UUID
	projectID                  pgtype.UUID
	environmentID              pgtype.UUID
	workspaceID                pgtype.UUID
	deploymentSandboxID        pgtype.UUID
	baseVersionID              pgtype.UUID
	runtimeInstanceID          pgtype.UUID
	runtimeEpoch               int64
	reservationToken           string
	guestdChannelTokenHash     string
	state                      db.WorkspaceMountState
	runtimeID                  string
	sandboxImageArtifact       api.CASObject
	sandboxImageArtifactFormat string
	rootfsDigest               string
	imageDigest                string
	imageFormat                string
	workspaceArtifact          api.WorkerWorkspaceArtifact
	workspaceMountPath         string
	requestedMilliCPU          int64
	requestedMemoryMiB         int64
	requestedDiskMiB           int64
	requestedExecutionSlots    int32
	runtimeABI                 string
	guestdABI                  string
	adapterABI                 string
	fencingGeneration          int64
	expiresAt                  time.Time
}

func workerWorkspaceMountFromClaim(row db.ClaimWorkspaceMountRow) *api.WorkerWorkspaceMount {
	return workerWorkspaceMountFromFields(workerWorkspaceMountFields{
		id:                     row.ID,
		orgID:                  row.OrgID,
		projectID:              row.ProjectID,
		environmentID:          row.EnvironmentID,
		workspaceID:            row.WorkspaceID,
		deploymentSandboxID:    row.DeploymentSandboxID,
		baseVersionID:          row.BaseVersionID,
		runtimeInstanceID:      row.RuntimeInstanceID,
		runtimeEpoch:           row.RuntimeEpoch,
		reservationToken:       row.RuntimeInstanceToken,
		guestdChannelTokenHash: row.GuestdChannelTokenHash,
		state:                  row.State,
		runtimeID:              row.RuntimeID,
		sandboxImageArtifact: api.CASObject{
			Digest:    row.ImageArtifactDigest,
			SizeBytes: row.ImageArtifactSizeBytes,
			MediaType: row.ImageArtifactMediaType,
		},
		sandboxImageArtifactFormat: row.ImageArtifactFormat,
		rootfsDigest:               row.RootfsDigest,
		imageDigest:                row.ImageDigest,
		imageFormat:                row.ImageFormat,
		workspaceArtifact: api.WorkerWorkspaceArtifact{
			Digest:     row.WorkspaceArtifactDigest,
			MediaType:  row.WorkspaceArtifactMediaType,
			Encoding:   row.WorkspaceArtifactEncoding,
			SizeBytes:  row.WorkspaceArtifactSizeBytes,
			EntryCount: row.WorkspaceArtifactEntryCount,
		},
		workspaceMountPath:      row.WorkspaceMountPath,
		requestedMilliCPU:       int64(row.RequestedCpuMillis),
		requestedMemoryMiB:      int64(row.RequestedMemoryMib),
		requestedDiskMiB:        row.RequestedDiskMib,
		requestedExecutionSlots: row.RequestedExecutionSlots,
		runtimeABI:              row.RuntimeABI,
		guestdABI:               row.GuestdAbi,
		adapterABI:              row.AdapterAbi,
		fencingGeneration:       row.FencingGeneration,
		expiresAt:               row.GuestdChannelTokenExpiresAt.Time,
	})
}

func workerWorkspaceMountFromFields(fields workerWorkspaceMountFields) *api.WorkerWorkspaceMount {
	mount := &api.WorkerWorkspaceMount{
		ID:                         pgvalue.MustUUIDValue(fields.id).String(),
		OrgID:                      pgvalue.MustUUIDValue(fields.orgID).String(),
		ProjectID:                  pgvalue.MustUUIDValue(fields.projectID).String(),
		EnvironmentID:              pgvalue.MustUUIDValue(fields.environmentID).String(),
		WorkspaceID:                pgvalue.MustUUIDValue(fields.workspaceID).String(),
		DeploymentSandboxID:        pgvalue.MustUUIDValue(fields.deploymentSandboxID).String(),
		RuntimeInstanceID:          pgvalue.UUIDString(fields.runtimeInstanceID),
		RuntimeEpoch:               fields.runtimeEpoch,
		RuntimeInstanceToken:       fields.reservationToken,
		GuestdChannelTokenHash:     fields.guestdChannelTokenHash,
		State:                      string(fields.state),
		RuntimeID:                  fields.runtimeID,
		SandboxImageArtifact:       fields.sandboxImageArtifact,
		SandboxImageArtifactFormat: fields.sandboxImageArtifactFormat,
		RootfsDigest:               fields.rootfsDigest,
		ImageDigest:                fields.imageDigest,
		ImageFormat:                fields.imageFormat,
		WorkspaceArtifact:          fields.workspaceArtifact,
		WorkspaceMountPath:         fields.workspaceMountPath,
		RequestedMilliCPU:          fields.requestedMilliCPU,
		RequestedMemoryMiB:         fields.requestedMemoryMiB,
		RequestedDiskMiB:           fields.requestedDiskMiB,
		RequestedExecutionSlots:    fields.requestedExecutionSlots,
		RuntimeABI:                 fields.runtimeABI,
		GuestdABI:                  fields.guestdABI,
		AdapterABI:                 fields.adapterABI,
		FencingGeneration:          fields.fencingGeneration,
		ExpiresAt:                  fields.expiresAt,
	}
	if fields.baseVersionID.Valid {
		mount.BaseVersionID = pgvalue.MustUUIDValue(fields.baseVersionID).String()
	}
	return mount
}
