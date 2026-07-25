package control

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/secret"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	secretListDefaultLimit = int32(50)
	secretListMaxLimit     = int32(100)
	secretListCursorPrefix = "sec1."
)

type secretListCursor struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id"`
	Name          string `json:"name"`
	ID            string `json:"id"`
}

type secretAddress struct {
	id   uuid.UUID
	name string
}

func (s *Server) createSecret(w http.ResponseWriter, r *http.Request) {
	if s.secrets == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.CreateSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Secret create request JSON: %w", err)))
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
	writeJSON(w, http.StatusCreated, secretSnapshotResponse(record))
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	limit, cursor, err := parseSecretListQuery(r, scope.ProjectID, scope.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
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
	response := api.ListSecretsResponse{
		Secrets: make([]api.SecretResponse, 0, len(rows)),
	}
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
	s.getSecret(w, r, secretAddress{id: id})
}

func (s *Server) getSecretByName(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.getSecret(w, r, secretAddress{name: name})
}

func (s *Server) getSecret(w http.ResponseWriter, r *http.Request, address secretAddress) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("secret storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionSecretsWrite, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return
	}
	record, err := s.loadSecretSnapshot(r, environmentID, address)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errors.New("secret not found")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load secret"))
		return
	}
	writeJSON(w, http.StatusOK, secretSnapshotResponse(record))
}

func (s *Server) rotateSecretByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseSecretID(chi.URLParam(r, "secretID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.rotateSecret(w, r, secretAddress{id: id})
}

func (s *Server) rotateSecretByName(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.rotateSecret(w, r, secretAddress{name: name})
}

func (s *Server) rotateSecret(w http.ResponseWriter, r *http.Request, address secretAddress) {
	if s.secrets == nil || s.db == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RotateSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Secret rotate request JSON: %w", err)))
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
	record, err := s.loadSecretSnapshot(r, environmentID, address)
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
	writeJSON(w, http.StatusOK, secretSnapshotResponse(rotated))
}

func (s *Server) revokeSecretByID(w http.ResponseWriter, r *http.Request) {
	id, err := parseSecretID(chi.URLParam(r, "secretID"))
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.revokeSecret(w, r, secretAddress{id: id})
}

func (s *Server) revokeSecretByName(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := secret.ValidateName(name); err != nil {
		writeError(w, badRequest(err))
		return
	}
	s.revokeSecret(w, r, secretAddress{name: name})
}

func (s *Server) revokeSecret(w http.ResponseWriter, r *http.Request, address secretAddress) {
	if s.secrets == nil || s.db == nil {
		writeError(w, unavailable(errors.New("secret store is not configured")))
		return
	}
	var request api.RevokeSecretRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Secret revoke request JSON: %w", err)))
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
	record, err := s.loadSecretSnapshot(r, environmentID, address)
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
	writeJSON(w, http.StatusOK, secretSnapshotResponse(revoked))
}

func (s *Server) secretMutationAuthority(
	w http.ResponseWriter,
	r *http.Request,
) (auth.Actor, pgtype.UUID, bool) {
	actor := actorFromContext(r.Context())
	scope, _, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
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

func (s *Server) loadSecretSnapshot(
	r *http.Request,
	environmentID pgtype.UUID,
	address secretAddress,
) (db.GetSecretSnapshotRow, error) {
	if address.id != uuid.Nil {
		return s.db.GetSecretSnapshot(r.Context(), db.GetSecretSnapshotParams{
			EnvironmentID: environmentID,
			ID:            pgvalue.UUID(address.id),
		})
	}
	byName, err := s.db.GetSecretSnapshotByName(r.Context(), db.GetSecretSnapshotByNameParams{
		EnvironmentID: environmentID,
		Name:          address.name,
	})
	if err != nil {
		return db.GetSecretSnapshotRow{}, err
	}
	return db.GetSecretSnapshotRow(byName), nil
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
	if err := secret.ValidateName(cursor.Name); err != nil {
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
	return secretListCursorPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeSecretListCursor(raw string) (secretListCursor, error) {
	if !strings.HasPrefix(raw, secretListCursorPrefix) {
		return secretListCursor{}, errors.New("secret cursor is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(
		strings.TrimPrefix(raw, secretListCursorPrefix),
	)
	if err != nil {
		return secretListCursor{}, errors.New("secret cursor is invalid")
	}
	var cursor secretListCursor
	if json.Unmarshal(decoded, &cursor) != nil ||
		cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" ||
		cursor.Name == "" ||
		cursor.ID == "" {
		return secretListCursor{}, errors.New("secret cursor is invalid")
	}
	return cursor, nil
}

func parseSecretID(raw string) (uuid.UUID, error) {
	if strings.TrimSpace(raw) != raw {
		return uuid.Nil, errors.New("secret ID is invalid")
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil || id.String() != raw {
		return uuid.Nil, errors.New("secret ID is invalid")
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

func secretSnapshotResponse(record db.GetSecretSnapshotRow) api.SecretResponse {
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
