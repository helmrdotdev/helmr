package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	secretListDefaultLimit = int32(50)
	secretListMaxLimit     = int32(100)
)

type secretListCursor struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	ID            string `json:"id"`
}

func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.CreateSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid secret create request JSON: %w", err)))
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if err := secret.ValidateName(request.Name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor, environmentID, ok := s.secretMutationAuthority(w, r)
	if !ok {
		return
	}
	record, err := s.secrets.Create(
		r.Context(),
		pgvalue.MustUUIDValue(environmentID),
		request.Name,
		[]byte(request.Value),
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, actor, request.Name, "create", err)
		return
	}
	response, err := secretSnapshotResponse(record)
	if err != nil {
		writeError(w, errors.New("project secret"))
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, cursor, exactName, err := parseSecretListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	response := api.ListSecretsResponse{Secrets: []api.SecretResponse{}}
	if exactName != "" {
		row, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
			EnvironmentID: environmentID, Name: exactName,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, response)
			return
		}
		if err != nil {
			writeError(w, errors.New("load secret"))
			return
		}
		projected, err := secretSnapshotResponse(db.GetSecretSnapshotRow(row))
		if err != nil {
			writeError(w, errors.New("project secret"))
			return
		}
		response.Secrets = append(response.Secrets, projected)
		writeJSON(w, http.StatusOK, response)
		return
	}
	var afterName pgtype.Text
	var afterID pgtype.UUID
	if cursor != nil {
		afterName = pgvalue.Text(cursor.Name)
		afterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, err := s.db.ListSecrets(r.Context(), db.ListSecretsParams{
		EnvironmentID: environmentID,
		AfterName:     afterName,
		AfterID:       afterID,
		RowLimit:      limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list secrets"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response.Secrets = make([]api.SecretResponse, 0, len(rows))
	for _, row := range rows {
		projected, err := secretResponse(
			row.ID,
			row.Name,
			row.State,
			row.CreatedAt,
			row.RotatedAt,
			row.RevokedAt,
		)
		if err != nil {
			writeError(w, errors.New("project secret"))
			return
		}
		response.Secrets = append(response.Secrets, projected)
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeSecretListCursor(secretListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			Name: last.Name, ID: pgvalue.MustUUIDValue(last.ID).String(),
		})
		if err != nil {
			s.log.Error("encode Secret cursor failed", "error", err)
			writeError(w, errors.New("list secrets"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSecretByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseSecretID(chi.URLParam(r, "secretID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.getSecret(w, r, id)
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	record, err := s.db.GetSecretSnapshot(r.Context(), db.GetSecretSnapshotParams{
		EnvironmentID: environmentID, ID: pgvalue.UUID(id),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load secret"))
		return
	}
	response, err := secretSnapshotResponse(record)
	if err != nil {
		writeError(w, errors.New("project secret"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) rotateSecretByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseSecretID(chi.URLParam(r, "secretID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.rotateSecret(w, r, id)
}

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if s.secrets == nil || s.db == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RotateSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid secret rotate request JSON: %w", err)))
		return
	}
	idempotencyKey, err := requiredIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor, environmentID, ok := s.secretMutationAuthority(w, r)
	if !ok {
		return
	}
	record, err := s.db.GetSecretSnapshot(r.Context(), db.GetSecretSnapshotParams{
		EnvironmentID: environmentID, ID: pgvalue.UUID(id),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load secret"))
		return
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	rotated, err := s.secrets.Rotate(
		r.Context(),
		pgvalue.MustUUIDValue(environmentID),
		secretID,
		[]byte(request.Value),
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, actor, record.Name, "rotate", err)
		return
	}
	response, err := secretSnapshotResponse(rotated)
	if err != nil {
		writeError(w, errors.New("project secret"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) revokeSecretByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseSecretID(chi.URLParam(r, "secretID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.revokeSecret(w, r, id)
}

func (s *Server) revokeSecret(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	if s.secrets == nil || s.db == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RevokeSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid secret revoke request JSON: %w", err)))
		return
	}
	idempotencyKey, err := requiredIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	actor, environmentID, ok := s.secretMutationAuthority(w, r)
	if !ok {
		return
	}
	record, err := s.db.GetSecretSnapshot(r.Context(), db.GetSecretSnapshotParams{
		EnvironmentID: environmentID, ID: pgvalue.UUID(id),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load secret"))
		return
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	revoked, err := s.secrets.Revoke(
		r.Context(),
		pgvalue.MustUUIDValue(environmentID),
		secretID,
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, actor, record.Name, "revoke", err)
		return
	}
	response, err := secretSnapshotResponse(revoked)
	if err != nil {
		writeError(w, errors.New("project secret"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) secretMutationAuthority(
	w http.ResponseWriter,
	r *http.Request,
) (auth.Actor, pgtype.UUID, bool) {
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return auth.Actor{}, pgtype.UUID{}, false
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return auth.Actor{}, pgtype.UUID{}, false
	}
	return actor, environmentID, true
}

func (s *Server) writeSecretMutationError(
	w http.ResponseWriter,
	actor auth.Actor,
	name string,
	operation string,
	err error,
) {
	var conflictErr idempotency.ConflictError
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &conflictErr):
		writeError(w, conflict(conflictErr))
	case secret.IsUnavailable(err):
		writeError(w, conflict(err))
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, conflict(errors.New("secret name already exists")))
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, notFound(errors.New("secret not found")))
	default:
		s.log.Error(
			operation+" Secret failed",
			"org_id", actor.OrgID,
			"name", name,
			"error", err,
		)
		writeError(w, fmt.Errorf("%s secret", operation))
	}
}

func parseSecretListQuery(
	r *http.Request,
	projectID string,
	environmentID string,
) (int32, *secretListCursor, string, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" && name != "name" {
			return 0, nil, "", fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, "", fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	if name := values.Get("name"); name != "" {
		if values.Has("cursor") || values.Has("limit") {
			return 0, nil, "", errors.New("cursor and limit are not allowed with name")
		}
		if err := secret.ValidateName(name); err != nil {
			return 0, nil, "", err
		}
		return 0, nil, name, nil
	}
	limit := secretListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(secretListMaxLimit) {
			return 0, nil, "", fmt.Errorf(
				"limit must be an integer in [1,%d]",
				secretListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, "", nil
	}
	cursor, err := decodeSecretListCursor(rawCursor)
	if err != nil {
		return 0, nil, "", err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, "", errors.New("secret cursor belongs to another scope")
	}
	if err := secret.ValidateName(cursor.Name); err != nil {
		return 0, nil, "", errors.New("secret cursor is invalid")
	}
	if _, err := parseSecretID(cursor.ID); err != nil {
		return 0, nil, "", errors.New("secret cursor is invalid")
	}
	return limit, &cursor, "", nil
}

func encodeSecretListCursor(cursor secretListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSecretListCursor(raw string) (secretListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return secretListCursor{}, errors.New("secret cursor is invalid")
	}
	var cursor secretListCursor
	if json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" ||
		cursor.Name == "" ||
		ids.Validate(cursor.ID) != nil {
		return secretListCursor{}, errors.New("secret cursor is invalid")
	}
	return cursor, nil
}

func parseSecretID(raw string) (uuid.UUID, error) {
	id, err := ids.Parse(raw)
	if err != nil {
		return uuid.Nil(), errors.New("secret ID is invalid")
	}
	return id, nil
}

func requiredIdempotencyKey(raw string) (string, error) {
	value, err := normalizeIdempotencyKey(raw)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", errors.New("idempotency_key is required")
	}
	return value, nil
}

func secretSnapshotResponse(record db.GetSecretSnapshotRow) (api.SecretResponse, error) {
	return secretResponse(
		record.ID,
		record.Name,
		record.State,
		record.CreatedAt,
		record.RotatedAt,
		record.RevokedAt,
	)
}

func secretResponse(
	id pgtype.UUID,
	name string,
	state string,
	createdAt pgtype.Timestamptz,
	rotatedAt pgtype.Timestamptz,
	revokedAt pgtype.Timestamptz,
) (api.SecretResponse, error) {
	status, err := secretPublicStatus(state)
	if err != nil {
		return api.SecretResponse{}, err
	}
	return api.SecretResponse{
		ID:        pgvalue.MustUUIDValue(id).String(),
		Name:      name,
		Status:    status,
		CreatedAt: pgvalue.Time(createdAt),
		RotatedAt: pgvalue.TimePtr(rotatedAt),
		RevokedAt: pgvalue.TimePtr(revokedAt),
	}, nil
}

func secretPublicStatus(state string) (api.SecretStatus, error) {
	switch state {
	case "active":
		return api.SecretStatusActive, nil
	case "revoked":
		return api.SecretStatusRevoked, nil
	default:
		return "", fmt.Errorf("secret state %q has no public projection", state)
	}
}
