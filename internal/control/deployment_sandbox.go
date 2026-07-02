package control

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) listSandboxes(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actorHasAnyPermission(actor, scope, workspaceReadPermissions()...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	rows, err := store.ListCurrentDeploymentSandboxes(r.Context(), db.ListCurrentDeploymentSandboxesParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
	})
	if err != nil {
		s.log.Error("list sandboxes failed", "error", err)
		writeError(w, errors.New("list sandboxes"))
		return
	}
	response := make([]api.SandboxResponse, 0, len(rows))
	for _, row := range rows {
		response = append(response, sandboxResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListSandboxesResponse{Sandboxes: response})
}

func (s *Server) getSandbox(w http.ResponseWriter, r *http.Request) {
	store, scope, projectID, environmentID, ok := s.loadCurrentDeploymentReadScope(w, r)
	if !ok {
		return
	}
	actor := actorFromContext(r.Context())
	if !actor.HasPermission(auth.PermissionRunsRead, scope) && !actor.HasPermission(auth.PermissionTasksDeploy, scope) && !actorHasAnyPermission(actor, scope, workspaceReadPermissions()...) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	sandboxID := strings.TrimSpace(chi.URLParam(r, "sandboxID"))
	if sandboxID == "" {
		writeError(w, badRequest(errors.New("sandbox_id is required")))
		return
	}
	row, err := store.GetCurrentDeploymentSandbox(r.Context(), db.GetCurrentDeploymentSandboxParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SandboxID:     sandboxID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("sandbox not found")))
		return
	}
	if err != nil {
		s.log.Error("get sandbox failed", "sandbox_id", sandboxID, "error", err)
		writeError(w, errors.New("get sandbox"))
		return
	}
	writeJSON(w, http.StatusOK, sandboxResponse(row))
}

func sandboxResponse(row db.DeploymentSandbox) api.SandboxResponse {
	return api.SandboxResponse{
		ID:                  pgvalue.MustUUIDValue(row.ID).String(),
		DeploymentID:        pgvalue.MustUUIDValue(row.DeploymentID).String(),
		SandboxID:           row.SandboxID,
		Fingerprint:         row.Fingerprint,
		ImageArtifactID:     pgvalue.MustUUIDValue(row.ImageArtifactID).String(),
		ImageArtifactFormat: row.ImageArtifactFormat,
		RootfsDigest:        row.RootfsDigest,
		ImageDigest:         row.ImageDigest,
		ImageFormat:         row.ImageFormat,
		WorkspaceMountPath:  row.WorkspaceMountPath,
		ResourceFloor:       json.RawMessage(row.ResourceFloor),
		DiskFloorMib:        int32(row.DiskFloorMib),
		NetworkPolicy:       json.RawMessage(row.NetworkPolicy),
		RuntimeABI:          row.RuntimeABI,
		GuestdABI:           row.GuestdAbi,
		AdapterABI:          row.AdapterAbi,
		FilesystemFormat:    row.FilesystemFormat,
		DefaultUID:          int8Response(row.DefaultUid),
		DefaultGID:          int8Response(row.DefaultGid),
		DefaultWorkdir:      row.DefaultWorkdir,
		ContractVersion:     row.ContractVersion,
		CreatedAt:           pgvalue.Time(row.CreatedAt),
	}
}
