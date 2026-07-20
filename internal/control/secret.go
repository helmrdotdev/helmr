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
	"github.com/helmrdotdev/helmr/internal/idempotency"
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
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	rows, err := s.db.ListSecrets(r.Context(), db.ListSecretsParams{
		EnvironmentID: environmentID,
		RowLimit:      secretListLimit,
	})
	if err != nil {
		writeError(w, errors.New("list secrets"))
		return
	}
	response := api.ListSecretsResponse{Secrets: make([]api.SecretResponse, 0, len(rows))}
	for _, row := range rows {
		response.Secrets = append(response.Secrets, secretResponse(
			row.ID,
			row.Name,
			row.State,
			row.CreatedAt,
			row.RotatedAt,
			row.RevokedAt,
		))
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
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	record, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
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
	writeJSON(w, http.StatusOK, secretResponse(
		record.ID,
		record.Name,
		record.State,
		record.CreatedAt,
		record.RotatedAt,
		record.RevokedAt,
	))
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
	record, err := s.secrets.PutScoped(r.Context(), actor.OrgID, pgvalue.MustUUIDValue(projectID), pgvalue.MustUUIDValue(environmentID), name, []byte(request.Value))
	if err != nil {
		s.log.Error("set secret failed", "name", name, "error", err)
		writeError(w, errors.New("set secret"))
		return
	}
	var rotatedAt pgtype.Timestamptz
	if record.StateVersion > 1 {
		rotatedAt = record.UpdatedAt
	}
	writeJSON(w, http.StatusOK, secretResponse(
		record.ID,
		record.Name,
		record.State,
		record.CreatedAt,
		rotatedAt,
		record.RevokedAt,
	))
}

func (s *Server) revokeSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil || s.db == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var request api.RevokeSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid secret revoke request JSON: %w", err)))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if idempotencyKey == "" {
		writeError(w, badRequest(errors.New("idempotency_key is required")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, r.URL.Query().Get("project_id"), r.URL.Query().Get("environment_id"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	snapshot, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
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
	record, err := s.secrets.Revoke(
		r.Context(),
		pgvalue.MustUUIDValue(environmentID),
		pgvalue.MustUUIDValue(snapshot.ID),
		idempotencyKey,
	)
	if err != nil {
		var conflictErr idempotency.ConflictError
		if errors.As(err, &conflictErr) {
			writeError(w, conflict(conflictErr))
			return
		}
		writeError(w, errors.New("revoke secret"))
		return
	}
	writeJSON(w, http.StatusOK, secretResponse(
		record.ID,
		record.Name,
		record.State,
		record.CreatedAt,
		record.RotatedAt,
		record.RevokedAt,
	))
}

func secretResponse(
	id pgtype.UUID,
	name string,
	state string,
	createdAt pgtype.Timestamptz,
	rotatedAt pgtype.Timestamptz,
	revokedAt pgtype.Timestamptz,
) api.SecretResponse {
	return api.SecretResponse{
		ID:        pgvalue.MustUUIDValue(id).String(),
		Name:      name,
		State:     state,
		CreatedAt: pgvalue.Time(createdAt),
		RotatedAt: pgvalue.TimePtr(rotatedAt),
		RevokedAt: pgvalue.TimePtr(revokedAt),
	}
}
