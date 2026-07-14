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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
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
	worker := workerFromContext(r.Context())
	guestdChannelToken, err := token.GenerateOpaque(32)
	if err != nil {
		writeError(w, errors.New("generate workspace mount guest channel token"))
		return
	}
	row, err := s.db.ClaimWorkspaceMount(r.Context(), db.ClaimWorkspaceMountParams{
		WorkerInstanceID:            pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:                 worker.WorkerEpoch,
		GuestdChannelTokenExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(workspaceMountReservationDuration), Valid: true},
		GuestdChannelTokenHash:      guestdChannelTokenHash(guestdChannelToken),
	})
	if isNoRows(err) {
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
	row, err := s.workerRenewWorkspaceMountTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
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
	row, err := s.workerMarkWorkspaceMountMountedTransition(r.Context(), request.OrgID, request.WorkspaceMountID)
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
	params, err := s.workerWorkspaceMountTransitionParams(r.Context(), request.OrgID, request.WorkspaceMountID)
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
		_, err := work.q.GetWorkspaceMountForWorkerPrimitiveScope(r.Context(), db.GetWorkspaceMountForWorkerPrimitiveScopeParams{
			OrgID: params.OrgID, WorkspaceID: pgvalue.UUID(workspaceID), ID: params.ID,
			WorkerInstanceID: params.WorkerInstanceID, WorkerEpoch: params.WorkerEpoch,
			RuntimeInstanceID: params.RuntimeInstanceID,
		})
		if isNoRows(err) {
			return conflict(codedError{code: "workspace_mount_capture_rejected", message: "workspace mount capture is stale"})
		}
		if err != nil {
			return errors.New("authorize workspace mount capture")
		}
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			OrgID:     params.OrgID,
			Digest:    digest,
			SizeBytes: request.ArtifactSizeBytes,
			MediaType: strings.TrimSpace(request.ArtifactMediaType),
		}); err != nil {
			return errors.New("record workspace capture CAS object")
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
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
			return errors.New("record workspace capture artifact")
		}
		var versionPublicID string
		version, err := createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.WorkspaceVersion, value: &versionPublicID}}, func() (db.PromoteWorkspaceMountStopCaptureRow, error) {
			return work.q.PromoteWorkspaceMountStopCapture(r.Context(), db.PromoteWorkspaceMountStopCaptureParams{
				OrgID: params.OrgID, ProjectID: pgvalue.UUID(projectID), EnvironmentID: pgvalue.UUID(environmentID),
				WorkspaceID: pgvalue.UUID(workspaceID), ID: params.ID,
				WorkerInstanceID: params.WorkerInstanceID, WorkerEpoch: params.WorkerEpoch,
				RuntimeInstanceID: params.RuntimeInstanceID, FencingGeneration: params.FencingGeneration,
				WorkspaceVersionID: pgvalue.UUID(uuid.Must(uuid.NewV7())), WorkspaceVersionPublicID: versionPublicID,
				ArtifactID: artifact.ID, ArtifactEncoding: strings.TrimSpace(request.ArtifactEncoding),
				ArtifactEntryCount: request.ArtifactEntryCount, ContentDigest: digest,
				SizeBytes: request.ArtifactSizeBytes, Message: "system capture before workspace stop",
			})
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
	params, err := s.workerWorkspaceMountTransitionParams(r.Context(), request.OrgID, request.WorkspaceMountID)
	if err != nil {
		writeError(w, err)
		return
	}
	var response api.WorkspaceMountResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		row, err := work.q.FailWorkspaceMount(r.Context(), db.FailWorkspaceMountParams{
			ReasonCode: pgtype.Text{String: "worker_mount_failed", Valid: true}, Error: errorJSON,
			OrgID: params.OrgID, ID: params.ID, WorkerInstanceID: params.WorkerInstanceID,
			WorkerEpoch: params.WorkerEpoch, RuntimeInstanceID: params.RuntimeInstanceID,
			FencingGeneration: params.FencingGeneration,
		})
		if isNoRows(err) {
			return conflict(errors.New("workspace mount is stale"))
		}
		if err != nil {
			return errors.New("fail workspace mount")
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

func guestdChannelTokenHash(token string) string {
	return sha256sum.HexBytes([]byte(strings.TrimSpace(token)))
}

func failedWorkspaceMountResponse(row db.WorkspaceMount) api.WorkspaceMountResponse {
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
	networkSlotID              pgtype.UUID
	networkSlotGeneration      int64
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
		runtimeEpoch:           row.WorkerEpoch,
		networkSlotID:          row.NetworkSlotID,
		networkSlotGeneration:  row.NetworkSlotGeneration,
		guestdChannelTokenHash: row.GuestdChannelTokenHash,
		state:                  row.State,
		runtimeID:              row.RuntimeID,
		sandboxImageArtifact: api.CASObject{
			Digest:    row.ImageDigest,
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
		requestedMilliCPU:       row.ReservedCpuMillis,
		requestedMemoryMiB:      row.ReservedMemoryBytes / (1024 * 1024),
		requestedDiskMiB:        row.ReservedWorkloadDiskBytes / (1024 * 1024),
		requestedExecutionSlots: row.ReservedExecutionSlots,
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
		NetworkSlotID:              pgvalue.UUIDString(fields.networkSlotID),
		NetworkSlotGeneration:      fields.networkSlotGeneration,
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
