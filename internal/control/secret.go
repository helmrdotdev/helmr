package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

const secretListLimit = int32(200)

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if _, err := s.requireEnvironmentPlacementWorkerGroup(r.Context(), s.db, actor.OrgID, projectID, environmentID); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.db.ListScopedSecrets(r.Context(), db.ListScopedSecretsParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		RowLimit:      secretListLimit,
	})
	if err != nil {
		writeError(w, errors.New("list secrets"))
		return
	}
	response := api.ListSecretsResponse{Secrets: make([]api.SecretResponse, 0, len(rows))}
	for _, row := range rows {
		response.Secrets = append(response.Secrets, secretResponse(row.ProjectID, row.EnvironmentID, row.Name, row.CreatedAt, row.UpdatedAt))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if _, err := s.requireEnvironmentPlacementWorkerGroup(r.Context(), s.db, actor.OrgID, projectID, environmentID); err != nil {
		writeError(w, err)
		return
	}
	record, err := s.db.GetScopedSecretMetadataByName(r.Context(), db.GetScopedSecretMetadataByNameParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load secret"))
		return
	}
	writeJSON(w, http.StatusOK, secretResponse(record.ProjectID, record.EnvironmentID, record.Name, record.CreatedAt, record.UpdatedAt))
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.SetSecretRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid secret request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if _, err := s.requireEnvironmentPlacementWorkerGroup(r.Context(), s.db, actor.OrgID, projectID, environmentID); err != nil {
		writeError(w, err)
		return
	}
	record, err := s.secrets.PutScoped(r.Context(), actor.OrgID, pgvalue.MustUUIDValue(projectID), pgvalue.MustUUIDValue(environmentID), name, []byte(request.Value))
	if err != nil {
		s.log.Error("set secret failed", "name", name, "error", err)
		writeError(w, errors.New("set secret"))
		return
	}
	writeJSON(w, http.StatusOK, secretResponse(record.ProjectID, record.EnvironmentID, record.Name, record.CreatedAt, record.UpdatedAt))
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	if _, err := s.requireEnvironmentPlacementWorkerGroup(r.Context(), s.db, actor.OrgID, projectID, environmentID); err != nil {
		writeError(w, err)
		return
	}
	rows, err := s.db.DeleteScopedSecret(r.Context(), db.DeleteScopedSecretParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		Name:          name,
	})
	if err != nil {
		writeError(w, errors.New("delete secret"))
		return
	}
	if rows == 0 {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func secretResponse(projectID pgtype.UUID, environmentID pgtype.UUID, name string, createdAt pgtype.Timestamptz, updatedAt pgtype.Timestamptz) api.SecretResponse {
	return api.SecretResponse{
		ProjectID:     pgvalue.MustUUIDValue(projectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID).String(),
		Name:          name,
		CreatedAt:     pgvalue.Time(createdAt),
		UpdatedAt:     pgvalue.Time(updatedAt),
	}
}
