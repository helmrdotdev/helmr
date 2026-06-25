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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/sha256sum"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

const workspaceMaterializationReservationDuration = 5 * time.Minute

func (s *Server) markStaleWorkspaceMaterializationsLost(ctx context.Context) error {
	_, err := s.db.MarkStaleWorkspaceMaterializationsLost(ctx, pgtype.Timestamptz{
		Time:  time.Now().Add(-workspaceMaterializationReservationDuration),
		Valid: true,
	})
	return err
}

func (s *Server) requestWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
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
	if err := s.markStaleWorkspaceMaterializationsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace materializations lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace materializations"))
		return
	}
	row, err := s.db.EnsureWorkspaceMaterializationRequested(r.Context(), db.EnsureWorkspaceMaterializationRequestedParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   pgvalue.UUID(workspaceID),
		Priority:      0,
		Request:       []byte(`{"source":"api"}`),
	})
	if isNoRows(err) {
		writeError(w, s.workspaceMaterializationPrerequisiteError(r.Context(), pgvalue.UUID(actor.OrgID), projectID, environmentID, pgvalue.UUID(workspaceID)))
		return
	}
	if err != nil {
		s.log.Error("ensure workspace materialization failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("ensure workspace materialization"))
		return
	}
	status := http.StatusOK
	if row.Inserted {
		status = http.StatusCreated
	}
	writeJSON(w, status, ensuredWorkspaceMaterializationResponse(row))
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
	if err := s.markStaleWorkspaceMaterializationsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace materializations lost failed", "workspace_id", workspaceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace materializations"))
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
	row, err := store.RequestWorkspaceMaterializationStop(ctx, db.RequestWorkspaceMaterializationStopParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	activeMaterialization := true
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
		activeMaterialization = false
	} else if err != nil {
		return api.WorkspaceStopResponse{}, err
	}
	response := workspaceStopResponse(workspaceID, row, activeMaterialization)
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

func workspaceStopResponse(workspaceID pgtype.UUID, row db.RequestWorkspaceMaterializationStopRow, activeMaterialization bool) api.WorkspaceStopResponse {
	if !activeMaterialization {
		return api.WorkspaceStopResponse{
			WorkspaceID: pgvalue.MustUUIDValue(workspaceID).String(),
			State:       "no_active_materialization",
		}
	}
	materialization := workspaceMaterializationResponse(workspaceMaterializationFromStopRow(row))
	return api.WorkspaceStopResponse{
		WorkspaceID:     pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		State:           string(row.State),
		Materialization: &materialization,
	}
}

func workspaceMaterializationFromStopRow(row db.RequestWorkspaceMaterializationStopRow) db.WorkspaceMaterialization {
	return db.WorkspaceMaterialization(row)
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

func (s *Server) workspaceMaterializationPrerequisiteError(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	return s.workspaceMaterializationPrerequisiteErrorWithStore(ctx, s.db, orgID, projectID, environmentID, workspaceID)
}

func (s *Server) workspaceMaterializationPrerequisiteErrorWithStore(ctx context.Context, store db.Querier, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, workspaceID pgtype.UUID) error {
	row, err := store.GetWorkspaceMaterializationPrerequisites(ctx, db.GetWorkspaceMaterializationPrerequisitesParams{
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkspaceID:   workspaceID,
	})
	if isNoRows(err) {
		return notFound(errors.New("workspace not found"))
	}
	if err != nil {
		return errors.New("check workspace materialization prerequisites")
	}
	if row.ActiveMaterializationState.Valid {
		switch row.ActiveMaterializationState.WorkspaceMaterializationState {
		case db.WorkspaceMaterializationStateRequested, db.WorkspaceMaterializationStateMaterializing, db.WorkspaceMaterializationStateRestoring, db.WorkspaceMaterializationStateRunning:
		default:
			return conflict(codedError{code: "workspace_materialization_not_runnable", message: "workspace has an active materialization that is not runnable"})
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
	return conflict(codedError{code: "workspace_materialization_prerequisite_failed", message: "workspace materialization prerequisites are not satisfied"})
}

func (s *Server) workerClaimWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationClaimRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization claim request JSON: %w", err)))
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
	if err := s.markStaleWorkspaceMaterializationsLost(r.Context()); err != nil {
		s.log.Error("mark stale workspace materializations lost failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("reap stale workspace materializations"))
		return
	}
	capacity, err := s.db.GetWorkerInstanceQueueCapacity(r.Context(), pgvalue.UUID(worker.WorkerInstanceID))
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMaterializationClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("worker capacity lookup failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("get worker capacity"))
		return
	}
	if capacity.AvailableExecutionSlots <= 0 || capacity.AvailableMilliCpu <= 0 || capacity.AvailableMemoryMib <= 0 || capacity.AvailableDiskMib <= 0 {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMaterializationClaimResponse{})
		return
	}
	token, err := newWorkspaceMaterializationReservationToken()
	if err != nil {
		writeError(w, errors.New("generate materialization reservation token"))
		return
	}
	guestdChannelToken, err := newWorkspaceMaterializationReservationToken()
	if err != nil {
		writeError(w, errors.New("generate materialization guest channel token"))
		return
	}
	row, err := s.db.ClaimWorkspaceMaterialization(r.Context(), db.ClaimWorkspaceMaterializationParams{
		AvailableCpuMillis:      int32(capacity.AvailableMilliCpu),
		AvailableMemoryMib:      int32(capacity.AvailableMemoryMib),
		AvailableDiskMib:        capacity.AvailableDiskMib,
		AvailableExecutionSlots: capacity.AvailableExecutionSlots,
		RootfsDigest:            capabilities.RootfsDigest,
		RuntimeABI:              capabilities.RuntimeABI,
		GuestdAbi:               currentGuestdABI,
		AdapterAbi:              currentAdapterABI,
		WorkerInstanceID:        pgvalue.UUID(worker.WorkerInstanceID),
		ReservationToken:        token,
		ReservationExpiresAt:    pgtype.Timestamptz{Time: time.Now().Add(workspaceMaterializationReservationDuration), Valid: true},
		GuestdChannelTokenHash:  workspaceMaterializationTokenHash(guestdChannelToken),
		RuntimeID:               capabilities.RuntimeID,
	})
	if isNoRows(err) {
		writeJSON(w, http.StatusOK, api.WorkerWorkspaceMaterializationClaimResponse{})
		return
	}
	if err != nil {
		s.log.Error("claim workspace materialization failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
		writeError(w, errors.New("claim workspace materialization"))
		return
	}
	materialization := workerWorkspaceMaterializationFromClaim(row)
	materialization.GuestdChannelToken = guestdChannelToken
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMaterializationClaimResponse{Materialization: materialization})
}

func (s *Server) workerRenewWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationRenewRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization renew request JSON: %w", err)))
		return
	}
	row, err := s.workerRenewMaterializationTransition(r.Context(), request.OrgID, request.MaterializationID, request.ReservationToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace materialization is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("renew workspace materialization"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMaterializationResponse(row))
}

func (s *Server) workerMarkWorkspaceMaterializationRunning(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationRunningRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization running request JSON: %w", err)))
		return
	}
	row, err := s.workerMarkMaterializationRunningTransition(r.Context(), request.OrgID, request.MaterializationID, request.ReservationToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace materialization is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("mark workspace materialization running"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMaterializationResponse(row))
}

func (s *Server) workerStopWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationStopRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization stop request JSON: %w", err)))
		return
	}
	row, err := s.workerStopMaterializationTransition(r.Context(), request.OrgID, request.MaterializationID, request.ReservationToken)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace materialization is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("stop workspace materialization"))
		return
	}
	writeJSON(w, http.StatusOK, workspaceMaterializationResponse(row))
}

func (s *Server) workerCaptureWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationCaptureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization capture request JSON: %w", err)))
		return
	}
	params, err := workerMaterializationTransitionParams(r.Context(), request.OrgID, request.MaterializationID, request.ReservationToken)
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
		writeError(w, errors.New("workspace materialization capture requires transactional store"))
		return
	}
	tx, beginErr := s.tx.Begin(r.Context())
	if beginErr != nil {
		writeError(w, errors.New("capture workspace materialization"))
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
	version, err := store.PromoteWorkspaceMaterializationStopCapture(r.Context(), db.PromoteWorkspaceMaterializationStopCaptureParams{
		OrgID:              params.OrgID,
		ID:                 params.ID,
		WorkspaceID:        pgvalue.UUID(workspaceID),
		WorkerInstanceID:   params.WorkerInstanceID,
		ReservationToken:   params.ReservationToken,
		ProjectID:          pgvalue.UUID(projectID),
		EnvironmentID:      pgvalue.UUID(environmentID),
		ArtifactID:         artifact.ID,
		SizeBytes:          request.ArtifactSizeBytes,
		ArtifactEncoding:   strings.TrimSpace(request.ArtifactEncoding),
		ContentDigest:      digest,
		VersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
		ArtifactEntryCount: request.ArtifactEntryCount,
		Message:            "system capture before workspace stop",
	})
	if isNoRows(err) {
		writeError(w, conflict(codedError{code: "workspace_materialization_capture_rejected", message: "workspace materialization capture is stale"}))
		return
	}
	if err != nil {
		writeError(w, errors.New("promote workspace materialization capture"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit workspace materialization capture"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerWorkspaceMaterializationCaptureResponse{
		VersionID: pgvalue.MustUUIDValue(version.ID).String(),
	})
}

func (s *Server) workerFailWorkspaceMaterialization(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWorkspaceMaterializationFailRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker workspace materialization fail request JSON: %w", err)))
		return
	}
	errorJSON, err := normalizedJSONObject(request.Error, "error")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	params, err := workerMaterializationTransitionParams(r.Context(), request.OrgID, request.MaterializationID, request.ReservationToken)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.tx == nil {
		writeError(w, errors.New("workspace materialization failure requires transactional store"))
		return
	}
	tx, beginErr := s.tx.Begin(r.Context())
	if beginErr != nil {
		writeError(w, errors.New("fail workspace materialization"))
		return
	}
	defer tx.Rollback(r.Context())
	store := db.New(tx)
	row, err := store.FailWorkspaceMaterialization(r.Context(), db.FailWorkspaceMaterializationParams{
		OrgID:            params.OrgID,
		ID:               params.ID,
		WorkerInstanceID: params.WorkerInstanceID,
		ReservationToken: params.ReservationToken,
		Error:            errorJSON,
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("workspace materialization is stale")))
		return
	}
	if err != nil {
		writeError(w, errors.New("fail workspace materialization"))
		return
	}
	if err := failQueuedRunsForWorkspaceMaterializationFailure(r.Context(), store, row, errorJSON); err != nil {
		writeError(w, errors.New("fail queued runs waiting for workspace materialization"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit workspace materialization failure"))
		return
	}
	writeJSON(w, http.StatusOK, failedWorkspaceMaterializationResponse(row))
}

type queuedRunFailer interface {
	ListQueuedRunsForWorkspaceMaterialization(context.Context, db.ListQueuedRunsForWorkspaceMaterializationParams) ([]pgtype.UUID, error)
	FailQueuedRun(context.Context, db.FailQueuedRunParams) error
}

func failQueuedRunsForWorkspaceMaterializationFailure(ctx context.Context, store queuedRunFailer, row db.FailWorkspaceMaterializationRow, errorJSON json.RawMessage) error {
	runIDs, err := store.ListQueuedRunsForWorkspaceMaterialization(ctx, db.ListQueuedRunsForWorkspaceMaterializationParams{
		OrgID:                      row.OrgID,
		WorkspaceID:                row.WorkspaceID,
		WorkspaceMaterializationID: row.ID,
	})
	if err != nil {
		return err
	}
	message := workspaceMaterializationFailureRunMessage(errorJSON)
	reason, err := workspaceMaterializationFailureRunReason(row, message, errorJSON)
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

func workspaceMaterializationFailureRunMessage(errorJSON json.RawMessage) string {
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
	return "workspace materialization failed"
}

func workspaceMaterializationFailureRunReason(row db.FailWorkspaceMaterializationRow, message string, errorJSON json.RawMessage) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"origin":             "workspace_materialization",
		"message":            message,
		"workspace_id":       pgvalue.MustUUIDValue(row.WorkspaceID).String(),
		"materialization_id": pgvalue.MustUUIDValue(row.ID).String(),
		"error":              json.RawMessage(errorJSON),
	})
	if err != nil {
		return nil, err
	}
	return body, nil
}

func (s *Server) workerRenewMaterializationTransition(ctx context.Context, orgID string, materializationID string, reservationToken string) (db.WorkspaceMaterialization, error) {
	params, err := workerRenewMaterializationTransitionParams(ctx, orgID, materializationID, reservationToken)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	return s.db.RenewWorkspaceMaterialization(ctx, params)
}

func (s *Server) workerMarkMaterializationRunningTransition(ctx context.Context, orgID string, materializationID string, reservationToken string) (db.WorkspaceMaterialization, error) {
	params, err := workerRunningMaterializationTransitionParams(ctx, orgID, materializationID, reservationToken)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	if s.tx == nil {
		return db.WorkspaceMaterialization{}, errors.New("workspace materialization running transition requires transactional store")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	store := db.New(tx)
	row, err := store.MarkWorkspaceMaterializationRunning(ctx, params)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	if err := enqueuePendingWorkspacePrimitiveOperations(ctx, store, row); err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	return row, nil
}

func (s *Server) workerStopMaterializationTransition(ctx context.Context, orgID string, materializationID string, reservationToken string) (db.WorkspaceMaterialization, error) {
	params, err := workerMaterializationTransitionParams(ctx, orgID, materializationID, reservationToken)
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	row, err := s.db.StopWorkspaceMaterialization(ctx, db.StopWorkspaceMaterializationParams(params))
	if err != nil {
		return db.WorkspaceMaterialization{}, err
	}
	return stoppedWorkspaceMaterialization(row), nil
}

type workerMaterializationTransitionIDs struct {
	OrgID            pgtype.UUID
	ID               pgtype.UUID
	WorkerInstanceID pgtype.UUID
	ReservationToken string
}

func workerRenewMaterializationTransitionParams(ctx context.Context, orgID string, materializationID string, reservationToken string) (db.RenewWorkspaceMaterializationParams, error) {
	params, err := workerMaterializationTransitionParams(ctx, orgID, materializationID, reservationToken)
	if err != nil {
		return db.RenewWorkspaceMaterializationParams{}, err
	}
	return db.RenewWorkspaceMaterializationParams{
		OrgID:                params.OrgID,
		ID:                   params.ID,
		WorkerInstanceID:     params.WorkerInstanceID,
		ReservationToken:     params.ReservationToken,
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMaterializationReservationDuration)),
	}, nil
}

func workerRunningMaterializationTransitionParams(ctx context.Context, orgID string, materializationID string, reservationToken string) (db.MarkWorkspaceMaterializationRunningParams, error) {
	params, err := workerMaterializationTransitionParams(ctx, orgID, materializationID, reservationToken)
	if err != nil {
		return db.MarkWorkspaceMaterializationRunningParams{}, err
	}
	return db.MarkWorkspaceMaterializationRunningParams{
		OrgID:                params.OrgID,
		ID:                   params.ID,
		WorkerInstanceID:     params.WorkerInstanceID,
		ReservationToken:     params.ReservationToken,
		ReservationExpiresAt: pgvalue.Timestamptz(time.Now().Add(workspaceMaterializationReservationDuration)),
	}, nil
}

func workerMaterializationTransitionParams(ctx context.Context, orgID string, materializationID string, reservationToken string) (workerMaterializationTransitionIDs, error) {
	orgUUID, err := uuid.Parse(strings.TrimSpace(orgID))
	if err != nil {
		return workerMaterializationTransitionIDs{}, badRequest(errors.New("org_id must be a UUID"))
	}
	id, err := uuid.Parse(strings.TrimSpace(materializationID))
	if err != nil {
		return workerMaterializationTransitionIDs{}, badRequest(errors.New("materialization_id must be a UUID"))
	}
	token := strings.TrimSpace(reservationToken)
	if token == "" {
		return workerMaterializationTransitionIDs{}, badRequest(errors.New("reservation_token is required"))
	}
	worker := workerFromContext(ctx)
	return workerMaterializationTransitionIDs{
		OrgID:            pgvalue.UUID(orgUUID),
		ID:               pgvalue.UUID(id),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		ReservationToken: token,
	}, nil
}

func newWorkspaceMaterializationReservationToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func workspaceMaterializationTokenHash(token string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(token)))
}

func stoppedWorkspaceMaterialization(row db.StopWorkspaceMaterializationRow) db.WorkspaceMaterialization {
	return db.WorkspaceMaterialization(row)
}

func failedWorkspaceMaterializationResponse(row db.FailWorkspaceMaterializationRow) api.WorkspaceMaterializationResponse {
	return workspaceMaterializationResponse(db.WorkspaceMaterialization{
		ID:                   row.ID,
		ProjectID:            row.ProjectID,
		EnvironmentID:        row.EnvironmentID,
		WorkspaceID:          row.WorkspaceID,
		DeploymentSandboxID:  row.DeploymentSandboxID,
		BaseVersionID:        row.BaseVersionID,
		WorkerInstanceID:     row.WorkerInstanceID,
		ReservationExpiresAt: row.ReservationExpiresAt,
		State:                row.State,
		ClaimAttempt:         row.ClaimAttempt,
		FencingGeneration:    row.FencingGeneration,
		DirtyGeneration:      row.DirtyGeneration,
		LastHeartbeatAt:      row.LastHeartbeatAt,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	})
}

func workspaceMaterializationResponse(row db.WorkspaceMaterialization) api.WorkspaceMaterializationResponse {
	response := api.WorkspaceMaterializationResponse{
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
	if row.WorkerInstanceID.Valid {
		response.WorkerInstanceID = pgvalue.MustUUIDValue(row.WorkerInstanceID).String()
	}
	if row.ReservationExpiresAt.Valid {
		response.ReservationExpiresAt = &row.ReservationExpiresAt.Time
	}
	if row.LastHeartbeatAt.Valid {
		response.LastHeartbeatAt = &row.LastHeartbeatAt.Time
	}
	return response
}

func ensuredWorkspaceMaterializationResponse(row db.EnsureWorkspaceMaterializationRequestedRow) api.WorkspaceMaterializationResponse {
	return workspaceMaterializationResponse(db.WorkspaceMaterialization{
		ID:                   row.ID,
		ProjectID:            row.ProjectID,
		EnvironmentID:        row.EnvironmentID,
		WorkspaceID:          row.WorkspaceID,
		DeploymentSandboxID:  row.DeploymentSandboxID,
		BaseVersionID:        row.BaseVersionID,
		WorkerInstanceID:     row.WorkerInstanceID,
		ReservationExpiresAt: row.ReservationExpiresAt,
		State:                row.State,
		ClaimAttempt:         row.ClaimAttempt,
		FencingGeneration:    row.FencingGeneration,
		DirtyGeneration:      row.DirtyGeneration,
		LastHeartbeatAt:      row.LastHeartbeatAt,
		CreatedAt:            row.CreatedAt,
		UpdatedAt:            row.UpdatedAt,
	})
}

type workerWorkspaceMaterializationFields struct {
	id                         pgtype.UUID
	orgID                      pgtype.UUID
	projectID                  pgtype.UUID
	environmentID              pgtype.UUID
	workspaceID                pgtype.UUID
	deploymentSandboxID        pgtype.UUID
	baseVersionID              pgtype.UUID
	reservationToken           string
	guestdChannelTokenHash     string
	state                      db.WorkspaceMaterializationState
	runtimeID                  string
	sandboxImageArtifact       api.CASObject
	sandboxImageArtifactFormat string
	rootfsDigest               string
	imageDigest                string
	imageFormat                string
	workspaceArtifact          api.WorkerWorkspaceArtifact
	workspaceMountPath         string
	runtimeABI                 string
	guestdABI                  string
	adapterABI                 string
	fencingGeneration          int64
	expiresAt                  time.Time
}

func workerWorkspaceMaterializationFromClaim(row db.ClaimWorkspaceMaterializationRow) *api.WorkerWorkspaceMaterialization {
	return workerWorkspaceMaterializationFromFields(workerWorkspaceMaterializationFields{
		id:                     row.ID,
		orgID:                  row.OrgID,
		projectID:              row.ProjectID,
		environmentID:          row.EnvironmentID,
		workspaceID:            row.WorkspaceID,
		deploymentSandboxID:    row.DeploymentSandboxID,
		baseVersionID:          row.BaseVersionID,
		reservationToken:       row.ReservationToken,
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
		workspaceMountPath: row.WorkspaceMountPath,
		runtimeABI:         row.RuntimeABI,
		guestdABI:          row.GuestdAbi,
		adapterABI:         row.AdapterAbi,
		fencingGeneration:  row.FencingGeneration,
		expiresAt:          row.ReservationExpiresAt.Time,
	})
}

func workerWorkspaceMaterializationFromFields(fields workerWorkspaceMaterializationFields) *api.WorkerWorkspaceMaterialization {
	materialization := &api.WorkerWorkspaceMaterialization{
		ID:                         pgvalue.MustUUIDValue(fields.id).String(),
		OrgID:                      pgvalue.MustUUIDValue(fields.orgID).String(),
		ProjectID:                  pgvalue.MustUUIDValue(fields.projectID).String(),
		EnvironmentID:              pgvalue.MustUUIDValue(fields.environmentID).String(),
		WorkspaceID:                pgvalue.MustUUIDValue(fields.workspaceID).String(),
		DeploymentSandboxID:        pgvalue.MustUUIDValue(fields.deploymentSandboxID).String(),
		ReservationToken:           fields.reservationToken,
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
		RuntimeABI:                 fields.runtimeABI,
		GuestdABI:                  fields.guestdABI,
		AdapterABI:                 fields.adapterABI,
		FencingGeneration:          fields.fencingGeneration,
		ExpiresAt:                  fields.expiresAt,
	}
	if fields.baseVersionID.Valid {
		materialization.BaseVersionID = pgvalue.MustUUIDValue(fields.baseVersionID).String()
	}
	return materialization
}
