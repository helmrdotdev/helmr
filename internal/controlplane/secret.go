package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/access"
	"net/http"
	"strconv"
	"strings"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/helmrdotdev/helmr/internal/secretname"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// secretRevokedForNewWork refuses new work that needs a revoked raw Secret value.
const secretRevokedForNewWork = "A raw Secret binding is revoked; rotate the Secret to start new work"

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
		s.writeOperationError(w, r, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.CreateSecretRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid secret create request JSON: %w", err))
		return
	}
	request.Secret = strings.TrimSpace(request.Secret)
	if err := secretname.Validate(request.Secret); err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	principal, environmentID, ok := s.secretMutationAuthority(w, r, auth.PermissionSecretsCreate)
	if !ok {
		return
	}
	record, err := s.secrets.Create(
		r.Context(), principal,
		pgvalue.MustUUIDValue(environmentID),
		request.Secret,
		[]byte(request.Value),
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, r, principal, request.Secret, "create", err)
		return
	}
	response := secretSnapshotResponse(db.GetSecretSnapshotRow(record))
	writeJSON(w, http.StatusCreated, response)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionSecretsRead, scope,
		errors.New("permission is required")); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	limit, cursor, err := parseSecretListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	response := api.ListSecretsResponse{Secrets: []api.SecretResponse{}}
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
		s.writeOperationError(w, r, errors.New("list secrets"))
		return
	}
	hasMore := len(rows) > int(limit)
	if hasMore {
		rows = rows[:limit]
	}
	response.Secrets = make([]api.SecretResponse, 0, len(rows))
	for _, row := range rows {
		response.Secrets = append(response.Secrets, secretResponse(row.Name, row.CreatedAt, row.RotatedAt, row.RevokedAt))
	}
	if hasMore {
		last := rows[len(rows)-1]
		response.NextCursor, err = encodeSecretListCursor(secretListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID,
			Name: last.Name, ID: pgvalue.MustUUIDValue(last.ID).String(),
		})
		if err != nil {
			s.log.Error("encode Secret cursor failed", "error", err)
			s.writeOperationError(w, r, errors.New("list secrets"))
			return
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getSecretByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "secret")
	err := secretname.Validate(id)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	s.getSecret(w, r, id)
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, id string) {
	principal := principalFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	if permissionErr := auth.Require(principal, auth.PermissionSecretsRead, scope,
		errors.New("permission is required")); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return
	}
	record, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
		EnvironmentID: environmentID, Name: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeOperationError(w, r, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		s.writeOperationError(w, r, errors.New("load secret"))
		return
	}
	response := secretSnapshotResponse(db.GetSecretSnapshotRow(record))
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) rotateSecretByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "secret")
	err := secretname.Validate(id)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	s.rotateSecret(w, r, id)
}

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request, id string) {
	if s.secrets == nil {
		s.writeOperationError(w, r, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RotateSecretRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid secret rotate request JSON: %w", err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	principal, environmentID, ok := s.secretMutationAuthority(w, r, auth.PermissionSecretsRotate)
	if !ok {
		return
	}
	record, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
		EnvironmentID: environmentID, Name: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeOperationError(w, r, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		s.writeOperationError(w, r, errors.New("load secret"))
		return
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	rotated, err := s.secrets.Rotate(
		r.Context(), principal,
		pgvalue.MustUUIDValue(environmentID),
		secretID,
		[]byte(request.Value),
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, r, principal, record.Name, "rotate", err)
		return
	}
	response := secretSnapshotResponse(rotated)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) revokeSecretByID(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "secret")
	err := secretname.Validate(id)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	s.revokeSecret(w, r, id)
}

func (s *Server) revokeSecret(w http.ResponseWriter, r *http.Request, id string) {
	if s.secrets == nil {
		s.writeOperationError(w, r, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RevokeSecretRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		s.writeOperationError(w, r, fmt.Errorf("invalid secret revoke request JSON: %w", err))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return
	}
	principal, environmentID, ok := s.secretMutationAuthority(w, r, auth.PermissionSecretsRevoke)
	if !ok {
		return
	}
	record, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
		EnvironmentID: environmentID, Name: id,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		s.writeOperationError(w, r, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		s.writeOperationError(w, r, errors.New("load secret"))
		return
	}
	secretID := pgvalue.MustUUIDValue(record.ID)
	revoked, err := s.secrets.Revoke(
		r.Context(), principal,
		pgvalue.MustUUIDValue(environmentID),
		secretID,
		idempotencyKey,
	)
	if err != nil {
		s.writeSecretMutationError(w, r, principal, record.Name, "revoke", err)
		return
	}
	response := secretSnapshotResponse(revoked)
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) secretMutationAuthority(
	w http.ResponseWriter,
	r *http.Request,
	permission auth.Permission,
) (auth.Principal, pgtype.UUID, bool) {
	principal := principalFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		s.writeOperationError(w, r, badRequest(err))
		return auth.Principal{}, pgtype.UUID{}, false
	}
	if permissionErr := auth.Require(principal, permission, scope,
		errors.New("permission is required")); permissionErr != nil {
		s.writeOperationError(w, r, forbidden(permissionErr))
		return auth.Principal{}, pgtype.UUID{}, false
	}
	return principal, environmentID, true
}

func (s *Server) writeSecretMutationError(
	w http.ResponseWriter,
	r *http.Request,
	principal auth.Principal,
	name string,
	operation string,
	err error,
) {
	if errors.Is(err, audit.ErrCapture) {
		s.writeOperationError(w, r, err)
		return
	}
	var expired idempotency.ExpiredError
	var retryConflict idempotency.ConflictError
	switch {
	case errors.Is(err, access.ErrDenied):
		s.writeOperationError(w, r, forbidden(errors.Join(errors.New("permission is required"), err)))
	case errors.As(err, &expired):
		s.writeOperationError(w, r, gone(expired))
	case errors.As(err, &retryConflict):
		s.writeOperationError(w, r, conflict(retryConflict))
	case errors.Is(err, secret.ErrMutationConflict):
		s.writeOperationError(w, r, conflict(secret.ErrMutationConflict))
	case errors.Is(err, secret.ErrInvalidMutation):
		s.writeOperationError(w, r, badRequest(secret.ErrInvalidMutation))
	case secret.IsUnavailable(err):
		s.writeOperationError(w, r, conflict(err))
	case errors.Is(err, secret.ErrNameExists):
		s.writeOperationError(w, r, conflict(secret.ErrNameExists))
	case errors.Is(err, secret.ErrNameRevoked):
		s.writeOperationError(w, r, conflict(codedError{code: "secret_revoked", message: "A Secret with this name is revoked; rotate it to set a new value."}))
	case errors.Is(err, pgx.ErrNoRows):
		s.writeOperationError(w, r, notFound(errors.New("secret not found")))
	default:
		s.log.Error(
			operation+" Secret failed",
			"org_id", principal.OrgID,
			"name", name,
			"error", err,
		)
		s.writeOperationError(w, r, fmt.Errorf("%s secret", operation))
	}
}

func parseSecretListQuery(
	r *http.Request,
	projectID string,
	environmentID string,
) (int32, *secretListCursor, error) {
	values := r.URL.Query()
	for name, entries := range values {
		if name != "cursor" && name != "limit" {
			return 0, nil, fmt.Errorf("query parameter %q is not supported", name)
		}
		if len(entries) != 1 || strings.TrimSpace(entries[0]) == "" {
			return 0, nil, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	limit := secretListDefaultLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 32)
		if err != nil || parsed < 1 || parsed > int64(secretListMaxLimit) {
			return 0, nil, fmt.Errorf(
				"limit must be an integer in [1,%d]",
				secretListMaxLimit,
			)
		}
		limit = int32(parsed)
	}
	rawCursor := values.Get("cursor")
	if rawCursor == "" {
		return limit, nil, nil
	}
	cursor, err := decodeSecretListCursor(rawCursor)
	if err != nil {
		return 0, nil, err
	}
	if cursor.ProjectID != projectID || cursor.EnvironmentID != environmentID {
		return 0, nil, errors.New("secret cursor belongs to another scope")
	}
	if err := secretname.Validate(cursor.Name); err != nil {
		return 0, nil, errors.New("secret cursor is invalid")
	}
	if _, err := parseSecretID(cursor.ID); err != nil {
		return 0, nil, errors.New("secret cursor is invalid")
	}
	return limit, &cursor, nil
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

func secretSnapshotResponse(record db.GetSecretSnapshotRow) api.SecretResponse {
	return secretResponse(record.Name, record.CreatedAt, record.RotatedAt, record.RevokedAt)
}

// A Secret is revoked exactly while revoked_at is set.
func secretResponse(
	name string,
	createdAt pgtype.Timestamptz,
	rotatedAt pgtype.Timestamptz,
	revokedAt pgtype.Timestamptz,
) api.SecretResponse {
	status := api.SecretStatusActive
	if revokedAt.Valid {
		status = api.SecretStatusRevoked
	}
	createdAt.Time = createdAt.Time.UTC()
	rotatedAt.Time = rotatedAt.Time.UTC()
	revokedAt.Time = revokedAt.Time.UTC()
	return api.SecretResponse{
		Secret:    name,
		Status:    status,
		CreatedAt: pgvalue.Time(createdAt),
		RotatedAt: pgvalue.TimePtr(rotatedAt),
		RevokedAt: pgvalue.TimePtr(revokedAt),
	}
}
