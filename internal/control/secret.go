package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5/pgtype"
)

const secretListLimit = int32(200)

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("secret storage is not configured"))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, scoped, err := s.requestedSecretScope(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	var rows []db.ListScopedSecretsRow
	if scoped {
		rows, err = s.db.ListScopedSecrets(r.Context(), db.ListScopedSecretsParams{
			OrgID:         ids.ToPG(actor.OrgID),
			ProjectID:     projectID,
			EnvironmentID: environmentID,
			RowLimit:      secretListLimit,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("list secrets"))
			return
		}
	} else {
		defaultRows, err := s.db.ListSecrets(r.Context(), db.ListSecretsParams{
			OrgID:    ids.ToPG(actor.OrgID),
			RowLimit: secretListLimit,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("list secrets"))
			return
		}
		rows = make([]db.ListScopedSecretsRow, 0, len(defaultRows))
		for _, row := range defaultRows {
			rows = append(rows, db.ListScopedSecretsRow(row))
		}
	}
	response := api.ListSecretsResponse{Secrets: make([]api.SecretResponse, 0, len(rows))}
	for _, row := range rows {
		response.Secrets = append(response.Secrets, secretResponse(row.ProjectID, row.EnvironmentID, row.Name, row.CreatedAt, row.UpdatedAt))
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) setSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("secret store is not configured"))
		return
	}
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var request api.SetSecretRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid secret request JSON: %w", err))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.secretRequestScope(r.Context(), actor.OrgID, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	record, err := s.secrets.PutScoped(r.Context(), actor.OrgID, ids.MustFromPG(projectID), ids.MustFromPG(environmentID), name, []byte(request.Value))
	if err != nil {
		s.log.Error("set secret failed", "name", name, "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("set secret"))
		return
	}
	writeJSON(w, http.StatusOK, secretResponse(record.ProjectID, record.EnvironmentID, record.Name, record.CreatedAt, record.UpdatedAt))
}

func (s *Server) requestedSecretScope(r *http.Request) (auth.Scope, pgtype.UUID, pgtype.UUID, bool, error) {
	actor := actorFromContext(r.Context())
	projectID := r.URL.Query().Get("project_id")
	environmentID := r.URL.Query().Get("environment_id")
	if projectID == "" && environmentID == "" {
		return auth.DefaultScope(actor.OrgID), pgtype.UUID{}, pgtype.UUID{}, false, nil
	}
	scope, scopeProjectID, scopeEnvironmentID, err := s.secretRequestScope(r.Context(), actor.OrgID, projectID, environmentID)
	return scope, scopeProjectID, scopeEnvironmentID, true, err
}

func (s *Server) secretRequestScope(ctx context.Context, orgID uuid.UUID, projectID string, environmentID string) (auth.Scope, pgtype.UUID, pgtype.UUID, error) {
	projectID = strings.TrimSpace(projectID)
	environmentID = strings.TrimSpace(environmentID)
	if projectID == "" {
		projectID = auth.DefaultProjectID
	}
	if environmentID == "" {
		environmentID = auth.DefaultEnvironmentID
	}
	scope, _, _, err := s.normalizeProjectEnvironmentScope(ctx, orgID, projectID, environmentID)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	scopeProjectID, scopeEnvironmentID, err := s.runScopeIDs(ctx, orgID, scope)
	if err != nil {
		return auth.Scope{}, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return scope, scopeProjectID, scopeEnvironmentID, nil
}

func secretResponse(projectID pgtype.UUID, environmentID pgtype.UUID, name string, createdAt pgtype.Timestamptz, updatedAt pgtype.Timestamptz) api.SecretResponse {
	return api.SecretResponse{
		ProjectID:     ids.MustFromPG(projectID).String(),
		EnvironmentID: ids.MustFromPG(environmentID).String(),
		Name:          name,
		CreatedAt:     pgTime(createdAt),
		UpdatedAt:     pgTime(updatedAt),
	}
}
