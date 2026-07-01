package control

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspaceMountReservationDuration             = 5 * time.Minute
	preparedRuntimeReservationDuration            = 2 * time.Minute
	capacityPressureCheckpointCommandPreemptLimit = int32(1)
	capacityPressureIdleStopPreemptLimit          = int32(1)
)

func (s *Server) markStaleWorkspaceMountsLost(ctx context.Context) error {
	_, err := s.db.MarkStaleWorkspaceMountsLost(ctx, pgtype.Timestamptz{
		Time:  time.Now().Add(-workspaceMountReservationDuration),
		Valid: true,
	})
	return err
}

func (s *Server) releaseExpiredPreparedRuntimeReservations(ctx context.Context) error {
	_, err := s.db.ReleaseExpiredPreparedRuntimeReservations(ctx, pgtype.Timestamptz{
		Time:  time.Now(),
		Valid: true,
	})
	return err
}

func (s *Server) requestWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "workspaceID")))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_id must be a UUID")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceMaterializeRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace materialize request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actorHasAnyPermission(actor, scope,
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesRead,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsRead,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionExecRead,
		auth.PermissionExecCreate,
		auth.PermissionExecManage,
		auth.PermissionPtyRead,
		auth.PermissionPtyCreate,
		auth.PermissionPtyManage,
		auth.PermissionPortsRead,
		auth.PermissionPortsExpose,
		auth.PermissionPortsClose,
	) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	row, err := s.db.EnsureWorkspaceMountRequested(r.Context(), db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           pgvalue.UUID(actor.OrgID),
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		WorkspaceID:     pgvalue.UUID(workspaceID),
		RequestPriority: 0,
		Request:         []byte(`{"source":"api"}`),
	})
	if isNoRows(err) {
		if s.log != nil {
			s.log.Info("workspace mount request blocked",
				"source", "api",
				"org_id", actor.OrgID.String(),
				"project_id", pgvalue.UUIDString(projectID),
				"environment_id", pgvalue.UUIDString(environmentID),
				"workspace_id", workspaceID.String(),
				"outcome", "blocked",
			)
		}
		writeError(w, s.workspaceMountPrerequisiteError(r.Context(), pgvalue.UUID(actor.OrgID), projectID, environmentID, pgvalue.UUID(workspaceID)))
		return
	}
	if err != nil {
		s.log.Error("ensure workspace mount failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("ensure workspace mount"))
		return
	}
	if s.log != nil {
		s.log.Info("workspace mount request ensured",
			"source", "api",
			"org_id", actor.OrgID.String(),
			"project_id", pgvalue.UUIDString(projectID),
			"environment_id", pgvalue.UUIDString(environmentID),
			"workspace_id", workspaceID.String(),
			"workspace_mount_id", pgvalue.UUIDString(row.ID),
			"state", row.State,
			"priority", row.Priority,
			"inserted", row.Inserted,
			"decision", row.Decision,
		)
	}
	status := http.StatusOK
	if row.Inserted {
		status = http.StatusCreated
	}
	writeJSON(w, status, ensuredWorkspaceMountResponse(row))
}

func (s *Server) stopWorkspace(w http.ResponseWriter, r *http.Request) {
	workspaceID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(r, "workspaceID")))
	if err != nil {
		writeError(w, badRequest(errors.New("workspace_id must be a UUID")))
		return
	}
	actor := actorFromContext(r.Context())
	var request api.WorkspaceStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid workspace stop request JSON: %w", err)))
		return
	}
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actorHasAnyPermission(actor, scope,
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionExecManage,
		auth.PermissionPtyManage,
		auth.PermissionPortsClose,
	) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	fingerprint, err := workspaceStopFingerprint()
	if err != nil {
		writeError(w, errors.New("fingerprint workspace stop request"))
		return
	}
	if err := s.markStaleWorkspaceMountsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace mounts lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace mounts"))
		return
	}
	response, err := s.requestWorkspaceStopForRequest(r.Context(), actor, projectID, environmentID, pgvalue.UUID(workspaceID), request, fingerprint)
	if err != nil {
		s.writeWorkspaceError(w, "request workspace stop", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func actorHasAnyPermission(actor auth.Actor, scope auth.Scope, permissions ...auth.Permission) bool {
	for _, permission := range permissions {
		if actor.HasPermission(permission, scope) {
			return true
		}
	}
	return false
}

func workspaceReadPermissions() []auth.Permission {
	return []auth.Permission{
		auth.PermissionWorkspaceLifecycleManage,
		auth.PermissionFilesRead,
		auth.PermissionFilesWrite,
		auth.PermissionVersionsRead,
		auth.PermissionVersionsCapture,
		auth.PermissionVersionsRestore,
		auth.PermissionVersionsDiff,
		auth.PermissionExecCreate,
		auth.PermissionExecRead,
		auth.PermissionExecManage,
		auth.PermissionPtyCreate,
		auth.PermissionPtyRead,
		auth.PermissionPtyManage,
		auth.PermissionPortsExpose,
		auth.PermissionPortsRead,
		auth.PermissionPortsClose,
	}
}

func (s *Server) requestWorkspaceStopForRequest(ctx context.Context, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID, request api.WorkspaceStopRequest, fingerprint string) (api.WorkspaceStopResponse, error) {
	if s.tx == nil {
		return api.WorkspaceStopResponse{}, errors.New("transactional workspace storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	idempotencyKey := strings.TrimSpace(request.IdempotencyKey)
	createdIdempotency := false
	if idempotencyKey != "" {
		idempotencyTTL, err := workspaceIdempotencyTTL(request.IdempotencyKeyTTL)
		if err != nil {
			return api.WorkspaceStopResponse{}, err
		}
		idempotency, err := ensureWorkspaceOperationIdempotency(ctx, store, db.EnsureWorkspaceOperationIdempotencyParams{
			ID:                   pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                pgvalue.UUID(actor.OrgID),
			ProjectID:            projectID,
			EnvironmentID:        environmentID,
			WorkspaceID:          workspaceID,
			OperationKind:        workspaceStopOperationKind,
			IdempotencyKey:       idempotencyKey,
			RequestFingerprint:   fingerprint,
			ResponseResourceType: "",
			ResponseResourceID:   pgtype.UUID{},
			ResponseBody:         []byte(`{}`),
			ExpiresAt:            pgvalue.Timestamptz(time.Now().Add(idempotencyTTL)),
		})
		if err != nil {
			return api.WorkspaceStopResponse{}, err
		}
		response, replayed, err := workspaceStopIdempotencyResponse(idempotency, fingerprint)
		if err != nil {
			return api.WorkspaceStopResponse{}, err
		}
		if replayed {
			return response, nil
		}
		createdIdempotency = true
	}
	row, err := store.RequestWorkspaceMountStop(ctx, db.RequestWorkspaceMountStopParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	activeMount := true
	if isNoRows(err) {
		_, err = store.SetWorkspaceDesiredStopped(ctx, db.SetWorkspaceDesiredStoppedParams{
			OrgID:         pgvalue.UUID(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			ID:            workspaceID,
		})
		if err != nil {
			return api.WorkspaceStopResponse{}, err
		}
		activeMount = false
	} else if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	response := workspaceStopResponse(workspaceID, row, activeMount)
	responseBody, err := json.Marshal(response)
	if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	if createdIdempotency {
		_, err = store.CompleteWorkspaceScopedOperationIdempotency(ctx, db.CompleteWorkspaceScopedOperationIdempotencyParams{
			OrgID:                pgvalue.UUID(actor.OrgID),
			ProjectID:            projectID,
			EnvironmentID:        environmentID,
			OperationKind:        workspaceStopOperationKind,
			WorkspaceID:          workspaceID,
			IdempotencyKey:       idempotencyKey,
			RequestFingerprint:   fingerprint,
			ResponseResourceType: "workspace",
			ResponseResourceID:   workspaceID,
			ResponseBody:         responseBody,
		})
		if err != nil {
			return api.WorkspaceStopResponse{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	return response, nil
}

func workspaceStopIdempotencyResponse(idempotency db.EnsureWorkspaceOperationIdempotencyRow, fingerprint string) (api.WorkspaceStopResponse, bool, error) {
	if idempotency.Inserted {
		return api.WorkspaceStopResponse{}, false, nil
	}
	if idempotency.RequestFingerprint != fingerprint {
		return api.WorkspaceStopResponse{}, false, errWorkspaceOperationIdempotencyUsed
	}
	if !idempotency.ResponseResourceID.Valid {
		return api.WorkspaceStopResponse{}, false, errWorkspaceOperationPending
	}
	var response api.WorkspaceStopResponse
	if err := json.Unmarshal(idempotency.ResponseBody, &response); err != nil {
		return api.WorkspaceStopResponse{}, false, fmt.Errorf("decode workspace stop idempotency response: %w", err)
	}
	return response, true, nil
}

func workspaceStopResponse(workspaceID pgtype.UUID, row db.RequestWorkspaceMountStopRow, activeMount bool) api.WorkspaceStopResponse {
	if !activeMount {
		return api.WorkspaceStopResponse{
			WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String(),
			State:       "no_active_mount",
		}
	}
	mount := workspaceMountResponse(workspaceMountFromStopRow(row))
	return api.WorkspaceStopResponse{
		WorkspaceID: pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		State:       string(row.State),
		Mount:       &mount,
	}
}

func workspaceMountFromStopRow(row db.RequestWorkspaceMountStopRow) db.WorkspaceMount {
	return db.WorkspaceMount(row)
}

func workspaceStopFingerprint() (string, error) {
	payload, err := json.Marshal(struct {
		Operation string `json:"operation"`
	}{
		Operation: string(workspaceStopOperationKind),
	})
	if err != nil {
		return "", err
	}
	return sha256sum.HexBytes(payload), nil
}

func (s *Server) workspaceMountPrerequisiteError(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	return s.workspaceMountPrerequisiteErrorWithStore(ctx, s.db, orgID, projectID, environmentID, workspaceID)
}

func (s *Server) workspaceMountPrerequisiteErrorWithStore(ctx context.Context, store db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	row, err := store.GetWorkspaceMountPrerequisites(ctx, db.GetWorkspaceMountPrerequisitesParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	if isNoRows(err) {
		return notFound(errors.New("workspace not found"))
	}
	if err != nil {
		return errors.New("check workspace mount prerequisites")
	}
	if row.ActiveMountState.Valid {
		switch row.ActiveMountState.WorkspaceMountState {
		case db.WorkspaceMountStateMounting, db.WorkspaceMountStateMounted:
		default:
			return conflict(codedError{code: "workspace_mount_not_runnable", message: "workspace has an active mount that is not runnable"})
		}
	}
	if !row.CurrentVersionID.Valid || !row.CurrentWorkspaceVersionID.Valid {
		return conflict(codedError{code: "workspace_version_missing", message: "workspace current version is missing"})
	}
	if row.CurrentWorkspaceVersionState.WorkspaceVersionState != db.WorkspaceVersionStateReady {
		return conflict(codedError{code: "workspace_version_not_ready", message: "workspace current version is not ready"})
	}
	if !row.CurrentWorkspaceArtifactID.Valid || !row.WorkspaceArtifactID.Valid {
		return conflict(codedError{code: "workspace_version_artifact_missing", message: "workspace current version artifact is missing"})
	}
	if !row.WorkspaceArtifactMediaType.Valid || row.WorkspaceArtifactMediaType.String != workspace.ArtifactMediaType {
		return conflict(codedError{code: "workspace_version_artifact_corrupt", message: "workspace current version artifact media type is invalid"})
	}
	if !row.SandboxImageArtifactID.Valid || !row.ImageArtifactID.Valid {
		return conflict(codedError{code: "deployment_sandbox_artifact_missing", message: "deployment sandbox image artifact is missing"})
	}
	if !row.ImageArtifactMediaType.Valid || row.ImageArtifactMediaType.String != api.SandboxImageArtifactMediaType {
		return conflict(codedError{code: "deployment_sandbox_artifact_corrupt", message: "deployment sandbox image artifact media type is invalid"})
	}
	return conflict(codedError{code: "workspace_mount_prerequisite_failed", message: "workspace mount prerequisites are not satisfied"})
}

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
	runtimeInstanceToken, err := auth.GenerateOpaqueToken(32)
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

func (s *Server) createCapacityPressureLiveRuntimeCheckpointWaitCommands(ctx context.Context, workerInstanceID uuid.UUID, trigger string) {
	if s.db == nil {
		return
	}
	commands, err := s.db.CreateCapacityPressureLiveRuntimeCheckpointWaitCommandsForWorker(ctx, db.CreateCapacityPressureLiveRuntimeCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		GuestdAbi:        currentGuestdABI,
		AdapterAbi:       currentAdapterABI,
		LimitCount:       capacityPressureCheckpointCommandPreemptLimit,
	})
	if err != nil {
		if s.log != nil {
			s.log.Warn("capacity pressure live wait checkpoint preemption failed",
				"worker_instance_id", workerInstanceID.String(),
				"trigger", trigger,
				"error", err,
			)
		}
		return
	}
	if s.log != nil && len(commands) > 0 {
		s.log.Info("capacity pressure live wait checkpoint commands created",
			"worker_instance_id", workerInstanceID.String(),
			"trigger", trigger,
			"command_count", len(commands),
		)
	}
}

func (s *Server) requestCapacityPressureIdleWorkspaceStops(ctx context.Context, workerInstanceID uuid.UUID, trigger string) {
	if s.db == nil {
		return
	}
	stops, err := s.db.RequestCapacityPressureIdleWorkspaceMountStopsForWorker(ctx, db.RequestCapacityPressureIdleWorkspaceMountStopsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		LimitCount:       capacityPressureIdleStopPreemptLimit,
	})
	if err != nil {
		if s.log != nil {
			s.log.Warn("capacity pressure idle workspace stop preemption failed",
				"worker_instance_id", workerInstanceID.String(),
				"trigger", trigger,
				"error", err,
			)
		}
		return
	}
	if s.log != nil && len(stops) > 0 {
		s.log.Info("capacity pressure idle workspace stops requested",
			"worker_instance_id", workerInstanceID.String(),
			"trigger", trigger,
			"mount_count", len(stops),
		)
	}
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

func (s *Server) workerStopWorkspaceMount(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMountStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace mount stop request JSON: %w", err)))
		return
	}
	row, err := s.workerStopWorkspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID, request.RuntimeInstanceToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stop workspace mount"))
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
	if s.tx == nil {
		writeError(w, errors.New("workspace mount capture requires transactional store"))
		return
	}
	tx, beginErr := s.tx.Begin(r.Context())
	if beginErr != nil {
		writeError(w, errors.New("capture workspace mount"))
		return
	}
	defer tx.Rollback(r.Context())
	store := db.New(tx)
	if _, err := store.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
		Digest:    digest,
		SizeBytes: request.ArtifactSizeBytes,
		MediaType: strings.TrimSpace(request.ArtifactMediaType),
	}); err != nil {
		writeError(w, errors.New("record workspace capture CAS object"))
		return
	}
	artifact, err := store.CreateArtifact(r.Context(), db.CreateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     params.OrgID,
		ProjectID:                 pgvalue.UUID(projectID),
		EnvironmentID:             pgvalue.UUID(environmentID),
		Digest:                    digest,
		Kind:                      db.ArtifactKindWorkspaceVersion,
		SizeBytes:                 request.ArtifactSizeBytes,
		MediaType:                 strings.TrimSpace(request.ArtifactMediaType),
		CreatedByWorkerInstanceID: params.WorkerInstanceID,
	})
	if err != nil {
		writeError(w, errors.New("record workspace capture artifact"))
		return
	}
	version, err := store.PromoteWorkspaceMountStopCapture(r.Context(), db.PromoteWorkspaceMountStopCaptureParams{
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
		writeError(w, conflict(codedError{code: "workspace_mount_capture_rejected", message: "workspace mount capture is stale"}))
		return
	}
	if err != nil {
		writeError(w, errors.New("promote workspace mount capture"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit workspace mount capture"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMountCaptureResponse{
		VersionID: pgvalue.MustUUIDValue(version.ID).String(),
	})
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
	if s.tx == nil {
		writeError(w, errors.New("workspace mount failure requires transactional store"))
		return
	}
	tx, beginErr := s.tx.Begin(r.Context())
	if beginErr != nil {
		writeError(w, errors.New("fail workspace mount"))
		return
	}
	defer tx.Rollback(r.Context())
	store := db.New(tx)
	row, err := store.FailWorkspaceMount(r.Context(), db.FailWorkspaceMountParams{
		OrgID:                params.OrgID,
		ID:                   params.ID,
		WorkerInstanceID:     params.WorkerInstanceID,
		RuntimeInstanceToken: params.RuntimeInstanceToken,
		Error:                errorJSON,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace mount is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("fail workspace mount"))
		return
	}
	if err := failQueuedRunsForWorkspaceMountFailure(r.Context(), store, row, errorJSON); err != nil {
		writeError(w, errors.New("fail queued runs waiting for workspace mount"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit workspace mount failure"))
		return
	}
	writeJSON(w, http.StatusOK, failedWorkspaceMountResponse(row))
}

type queuedRunFailer interface {
	ListQueuedRunsForWorkspaceMount(context.Context, db.ListQueuedRunsForWorkspaceMountParams) ([]pgtype.UUID, error)
	FailQueuedRun(context.Context, db.FailQueuedRunParams) error
}

func failQueuedRunsForWorkspaceMountFailure(ctx context.Context, store queuedRunFailer, row db.FailWorkspaceMountRow, errorJSON json.RawMessage) error {
	runIDs, err := store.ListQueuedRunsForWorkspaceMount(ctx, db.ListQueuedRunsForWorkspaceMountParams{
		OrgID:            row.OrgID,
		WorkspaceID:      row.WorkspaceID,
		WorkspaceMountID: row.ID,
	})
	if err != nil {
		return err
	}
	message := workspaceMountFailureRunMessage(errorJSON)
	reason, err := workspaceMountFailureRunReason(row, message, errorJSON)
	if err != nil {
		return err
	}
	for _, runID := range runIDs {
		if err := store.FailQueuedRun(ctx, db.FailQueuedRunParams{
			OrgID:        row.OrgID,
			RunID:        runID,
			ErrorMessage: message,
			Reason:       reason,
		}); err != nil {
			return err
		}
	}
	return nil
}

func workspaceMountFailureRunMessage(errorJSON json.RawMessage) string {
	var body struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	}
	if len(errorJSON) > 0 && json.Unmarshal(errorJSON, &body) == nil {
		if message := strings.TrimSpace(body.Message); message != "" {
			return message
		}
		if code := strings.TrimSpace(body.Code); code != "" {
			return code
		}
	}
	return "workspace mount failed"
}

func workspaceMountFailureRunReason(row db.FailWorkspaceMountRow, message string, errorJSON json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"origin":             "workspace_mount",
		"message":            message,
		"workspace_id":       pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		"workspace_mount_id": pgvalue.MustUUIDValue(row.ID).String(),
		"error":              json.RawMessage(errorJSON),
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (s *Server) workerRenewWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerRenewWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.RenewWorkspaceMount(ctx, params)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return db.WorkspaceMount(row), nil
}

func (s *Server) workerMarkWorkspaceMountMountedTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerMountedWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	if s.tx == nil {
		return db.WorkspaceMount{}, errors.New("workspace mount mounted transition requires transactional store")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	row, err := store.MarkWorkspaceMountMounted(ctx, params)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	mount := db.WorkspaceMount(row)
	if err := enqueuePendingWorkspacePrimitiveOperations(ctx, store, mount); err != nil {
		return db.WorkspaceMount{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspaceMount{}, err
	}
	return mount, nil
}

func (s *Server) workerStopWorkspaceMountTransition(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.WorkspaceMount, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	row, err := s.db.StopWorkspaceMount(ctx, db.StopWorkspaceMountParams{
		OrgID:                params.OrgID,
		ID:                   params.ID,
		WorkerInstanceID:     params.WorkerInstanceID,
		RuntimeInstanceToken: params.RuntimeInstanceToken,
	})
	if err != nil {
		return db.WorkspaceMount{}, err
	}
	return stoppedWorkspaceMount(row), nil
}

type workerWorkspaceMountTransitionIDs struct {
	OrgID                pgtype.UUID
	ID                   pgtype.UUID
	WorkerInstanceID     pgtype.UUID
	RuntimeInstanceToken string
}

func workerRenewWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.RenewWorkspaceMountParams, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.RenewWorkspaceMountParams{}, err
	}
	return db.RenewWorkspaceMountParams{
		OrgID:                       params.OrgID,
		ID:                          params.ID,
		WorkerInstanceID:            params.WorkerInstanceID,
		RuntimeInstanceToken:        params.RuntimeInstanceToken,
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	}, nil
}

func workerMountedWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (db.MarkWorkspaceMountMountedParams, error) {
	params, err := workerWorkspaceMountTransitionParams(ctx, orgID, workspaceMountID, reservationToken)
	if err != nil {
		return db.MarkWorkspaceMountMountedParams{}, err
	}
	return db.MarkWorkspaceMountMountedParams{
		OrgID:                       params.OrgID,
		ID:                          params.ID,
		WorkerInstanceID:            params.WorkerInstanceID,
		RuntimeInstanceToken:        params.RuntimeInstanceToken,
		GuestdChannelTokenExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMountReservationDuration)),
	}, nil
}

func workerWorkspaceMountTransitionParams(ctx context.Context, orgID string, workspaceMountID string, reservationToken string) (workerWorkspaceMountTransitionIDs, error) {
	orgUUID, err := uuid.Parse(strings.TrimSpace(orgID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("org_id must be a UUID"))
	}
	id, err := uuid.Parse(strings.TrimSpace(workspaceMountID))
	if err != nil {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("workspace_mount_id must be a UUID"))
	}
	token := strings.TrimSpace(reservationToken)
	if token == "" {
		return workerWorkspaceMountTransitionIDs{}, badRequest(errors.New("runtime_instance_token is required"))
	}
	worker := workerFromContext(ctx)
	return workerWorkspaceMountTransitionIDs{
		OrgID:                pgvalue.UUID(orgUUID),
		ID:                   pgvalue.UUID(id),
		WorkerInstanceID:     pgvalue.UUID(worker.WorkerInstanceID),
		RuntimeInstanceToken: token,
	}, nil
}

func newGuestdChannelToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func guestdChannelTokenHash(token string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(token)))
}

func stoppedWorkspaceMount(row db.StopWorkspaceMountRow) db.WorkspaceMount {
	return db.WorkspaceMount(row)
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

func workspaceMountResponse(row db.WorkspaceMount) api.WorkspaceMountResponse {
	response := api.WorkspaceMountResponse{
		ID:                  pgvalue.MustUUIDValue(row.ID).String(),
		ProjectID:           pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		WorkspaceID:         pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		DeploymentSandboxID: pgvalue.MustUUIDValue(row.DeploymentSandboxID).String(),
		State:               string(row.State),
		ClaimAttempt:        row.ClaimAttempt,
		FencingGeneration:   row.FencingGeneration,
		DirtyGeneration:     row.DirtyGeneration,
		CreatedAt:           row.CreatedAt.Time,
		UpdatedAt:           row.UpdatedAt.Time,
	}
	if row.BaseVersionID.Valid {
		response.BaseVersionID = pgvalue.MustUUIDValue(row.BaseVersionID).String()
	}
	if row.LastHeartbeatAt.Valid {
		response.LastHeartbeatAt = &row.LastHeartbeatAt.Time
	}
	return response
}

func ensuredWorkspaceMountResponse(row db.EnsureWorkspaceMountRequestedRow) api.WorkspaceMountResponse {
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
