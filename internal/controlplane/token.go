package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"uuid"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultTokenListLimit = int32(50)
	defaultTokenTimeout   = 10 * time.Minute
	maxTokenTimeout       = 365 * 24 * time.Hour
	tokenRequestBodyLimit = int64(1 << 20)
)

type tokenListCursor struct {
	ProjectID     string    `json:"project_id"`
	EnvironmentID string    `json:"environment_id"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	ID            string    `json:"id"`
}

var (
	errTokenNotFound           = codedError{code: "token_not_found", message: "token was not found"}
	errTokenExpired            = codedError{code: "token_expired", message: "token has expired"}
	errTokenCancelled          = codedError{code: "token_cancelled", message: "token was cancelled"}
	errTokenCompleted          = codedError{code: "token_completed", message: "token is already completed"}
	errTokenScopeDenied        = codedError{code: "token_scope_denied", message: "token credential is invalid"}
	errTokenCompletionConflict = codedError{code: "token_completion_conflict", message: "token completion conflicts with the existing result"}
)

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var request api.CreateTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid token create JSON: %w", err))
		return
	}
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionTokensCreate, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	timeoutMS := defaultTokenTimeout.Milliseconds()
	if strings.TrimSpace(request.Timeout) != "" {
		timeoutMS, err = api.ParseDurationMilliseconds(
			request.Timeout,
			"timeout",
			1,
			maxTokenTimeout.Milliseconds(),
		)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
	}
	metadata, tags, err := normalizeTokenAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token annotations: %w", err)))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	created, replayed, err := s.tokens.Create(r.Context(), token.CreateRequest{
		Scope:     token.Scope{OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID},
		TimeoutMS: timeoutMS, Metadata: metadata, Tags: tags, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	s.writeTokenCreated(w, created, replayed)
}

func (s *Server) workerCreateToken(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CreateTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid worker token create JSON: %w", err))
		return
	}
	parsed, err := parseRunLeaseFence(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	correlationID, err := parseCanonicalUUID("correlation_id", request.CorrelationID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	timeoutMS, err := normalizeTokenTimeout(request.TimeoutMS)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	metadata, tags, err := normalizeTokenAnnotations(request.Metadata, request.Tags)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token annotations: %w", err)))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if idempotencyKey == "" {
		idempotencyKey = "runtime:" + correlationID.String()
	}
	worker := workerFromContext(r.Context())
	created, replayed, err := s.tokens.CreateForRun(r.Context(), token.RunCreateRequest{
		Fence:     workerExecutionFence(worker, parsed, request.Lease),
		TimeoutMS: timeoutMS, Metadata: metadata, Tags: tags, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	s.writeTokenCreated(w, created, replayed)
}

// writeTokenCreated writes a created Token, 201 when it is new and 200 when
// its creation was replayed.
func (s *Server) writeTokenCreated(w http.ResponseWriter, created token.Created, replayed bool) {
	response, err := tokenCreateResponse(created.Token(), created.PublicAccessToken(), created.CallbackURL())
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}

func normalizeTokenTimeout(raw *int64) (int64, error) {
	if raw == nil {
		return defaultTokenTimeout.Milliseconds(), nil
	}
	if *raw < 1 || *raw > maxTokenTimeout.Milliseconds() {
		return 0, fmt.Errorf("timeout_ms must be between 1 and %d", maxTokenTimeout.Milliseconds())
	}
	return *raw, nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	query := r.URL.Query()
	if err := validateTokenListQuery(query); err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !principal.HasPermission(auth.PermissionTokensRead, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := optionalLimitQuery(r, defaultTokenListLimit)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	state := pgtype.Text{}
	statusFilter := ""
	if raw := strings.TrimSpace(query.Get("status")); raw != "" {
		switch db.TokenStatus(raw) {
		case db.TokenStatusPending, db.TokenStatusCompleted, db.TokenStatusExpired, db.TokenStatusCancelled:
			state = pgvalue.Text(raw)
			statusFilter = raw
		default:
			writeError(w, badRequest(errors.New("status must be pending, completed, expired, or cancelled")))
			return
		}
	}
	var cursor *tokenListCursor
	if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
		decoded, err := decodeTokenListCursor(raw)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		if decoded.ProjectID != scope.ProjectID || decoded.EnvironmentID != scope.EnvironmentID || decoded.Status != statusFilter {
			writeError(w, badRequest(errors.New("token cursor does not match request scope or filter")))
			return
		}
		cursor = &decoded
	}
	page := token.ListQuery{Status: state, Limit: limit}
	if cursor != nil {
		page.HasAfter = true
		page.AfterCreatedAt = pgvalue.Timestamptz(cursor.CreatedAt)
		page.AfterID = pgvalue.UUID(uuid.MustParse(cursor.ID))
	}
	rows, hasMore, err := token.List(r.Context(), s.db, token.Scope{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
	}, page)
	if err != nil {
		writeError(w, errors.New("list tokens"))
		return
	}
	var nextCursor string
	if hasMore {
		last := rows[len(rows)-1]
		nextCursor, err = encodeTokenListCursor(tokenListCursor{
			ProjectID: scope.ProjectID, EnvironmentID: scope.EnvironmentID, Status: statusFilter,
			CreatedAt: pgvalue.Time(last.CreatedAt), ID: pgvalue.UUIDString(last.ID),
		})
		if err != nil {
			writeError(w, errors.New("list tokens"))
			return
		}
	}
	tokens := make([]api.TokenListItem, 0, len(rows))
	for _, row := range rows {
		response, err := tokenListItem(row)
		if err != nil {
			writeError(w, errors.New("project token"))
			return
		}
		tokens = append(tokens, response)
	}
	writeJSON(w, http.StatusOK, api.ListTokensResponse{Tokens: tokens, NextCursor: nextCursor})
}

func validateTokenListQuery(query url.Values) error {
	for name := range query {
		switch name {
		case "cursor", "status", "limit":
		default:
			return fmt.Errorf("query parameter %q is not supported", name)
		}
	}
	for _, name := range []string{"cursor", "status", "limit"} {
		if len(query[name]) > 1 {
			return fmt.Errorf("%s must not be repeated", name)
		}
		if len(query[name]) == 1 && strings.TrimSpace(query[name][0]) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

func encodeTokenListCursor(cursor tokenListCursor) (string, error) {
	raw, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeTokenListCursor(raw string) (tokenListCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return tokenListCursor{}, errors.New("token cursor is invalid")
	}
	var cursor tokenListCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.ProjectID == "" ||
		cursor.EnvironmentID == "" || cursor.CreatedAt.IsZero() || ids.Validate(cursor.ID) != nil {
		return tokenListCursor{}, errors.New("token cursor is invalid")
	}
	return cursor, nil
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	tokenRow, ok := s.authorizeToken(w, r, auth.PermissionTokensRead)
	if !ok {
		return
	}
	response, err := tokenResponse(tokenRow)
	if err != nil {
		writeError(w, errors.New("project token"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) completeToken(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid token completion JSON: %w", err))
		return
	}
	if len(request.Result) == 0 {
		writeError(w, badRequest(errors.New("result is required")))
		return
	}
	tokenRow, ok := s.authorizeToken(w, r, auth.PermissionTokensComplete)
	if !ok {
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	completed, err := s.tokens.Complete(r.Context(), token.TargetOf(tokenRow), request.Result, idempotencyKey)
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	writeTokenResponse(w, completed)
}

func (s *Server) cancelToken(w http.ResponseWriter, r *http.Request) {
	var request api.CancelTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid token cancellation JSON: %w", err))
		return
	}
	tokenRow, ok := s.authorizeToken(w, r, auth.PermissionTokensCancel)
	if !ok {
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	cancelled, err := s.tokens.Cancel(r.Context(), token.TargetOf(tokenRow), idempotencyKey)
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	writeTokenResponse(w, cancelled)
}

func (s *Server) completeTokenWithCallback(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid token callback JSON: %w", err))
		return
	}
	if len(request.Result) == 0 {
		writeError(w, badRequest(errors.New("result is required")))
		return
	}
	tokenID, parseErr := ids.Parse(chi.URLParam(r, "tokenID"))
	callbackSecret := strings.TrimSpace(chi.URLParam(r, "callbackSecret"))
	if parseErr != nil || callbackSecret == "" {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	completed, err := s.tokens.CompleteWithCallback(r.Context(), tokenID, callbackSecret, request.Result)
	if err != nil {
		writeError(w, publicTokenError(err))
		return
	}
	writeTokenResponse(w, completed)
}

func (s *Server) completeTokenWithBearer(w http.ResponseWriter, r *http.Request) {
	s.writeTokenCORS(w)
	var request api.CompleteTokenRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		writeError(w, fmt.Errorf("invalid token completion JSON: %w", err))
		return
	}
	if len(request.Result) == 0 {
		writeError(w, badRequest(errors.New("result is required")))
		return
	}
	tokenID, parseErr := ids.Parse(chi.URLParam(r, "tokenID"))
	rawBearer, ok := bearerToken(r.Header.Get("Authorization"))
	if parseErr != nil || !ok {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	completed, err := s.tokens.CompleteWithBearer(r.Context(), tokenID, rawBearer, request.Result)
	if err != nil {
		writeError(w, publicTokenError(err))
		return
	}
	writeTokenResponse(w, completed)
}

func (s *Server) completeTokenBearerPreflight(w http.ResponseWriter, _ *http.Request) {
	s.writeTokenCORS(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// writeTokenResponse writes a Token's projection.
func writeTokenResponse(w http.ResponseWriter, row db.Token) {
	response, err := tokenResponse(row)
	if err != nil {
		writeError(w, errors.New("project token"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) authorizeToken(
	w http.ResponseWriter,
	r *http.Request,
	permission auth.Permission,
) (db.Token, bool) {
	tokenID, err := ids.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, notFound(errTokenNotFound))
		return db.Token{}, false
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return db.Token{}, false
	}
	if !principal.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.Token{}, false
	}
	tokenRow, err := token.Get(r.Context(), s.db, token.Scope{
		OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
	}, pgvalue.UUID(tokenID))
	if errors.Is(err, token.ErrNotFound) {
		writeError(w, notFound(errTokenNotFound))
		return db.Token{}, false
	}
	if err != nil {
		writeError(w, errors.New("load token"))
		return db.Token{}, false
	}
	return tokenRow, true
}

func tokenResponse(row db.Token) (api.TokenResponse, error) {
	status, err := tokenPublicStatus(row.Status)
	if err != nil {
		return api.TokenResponse{}, err
	}
	if !row.ExpiresAt.Valid {
		return api.TokenResponse{}, errors.New("token expiry is unavailable")
	}
	expiresAt := row.ExpiresAt.Time.UTC()
	var completedAt *time.Time
	if row.CompletedAt.Valid {
		value := row.CompletedAt.Time.UTC()
		completedAt = &value
	}
	response := api.TokenResponse{
		ID: pgvalue.UUIDString(row.ID), Status: status, TimeoutAt: expiresAt,
		Tags: append([]string{}, row.Tags...), Metadata: json.RawMessage(row.Metadata),
		CompletedAt: completedAt, CreatedAt: row.CreatedAt.Time.UTC(),
		UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
	if len(row.Result) > 0 {
		response.Result = json.RawMessage(row.Result)
	}
	return response, nil
}

func tokenListItem(row db.ListTokensRow) (api.TokenListItem, error) {
	status, err := tokenPublicStatus(row.Status)
	if err != nil {
		return api.TokenListItem{}, err
	}
	if !row.ExpiresAt.Valid || !row.CreatedAt.Valid || !row.UpdatedAt.Valid {
		return api.TokenListItem{}, errors.New("token list projection is invalid")
	}
	item := api.TokenListItem{
		ID: pgvalue.UUIDString(row.ID), Status: status,
		Tags: append([]string{}, row.Tags...), TimeoutAt: row.ExpiresAt.Time.UTC(),
		CreatedAt: row.CreatedAt.Time.UTC(), UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
	if row.CompletedAt.Valid {
		value := row.CompletedAt.Time.UTC()
		item.CompletedAt = &value
	}
	return item, nil
}

func tokenPublicStatus(state db.TokenStatus) (api.TokenStatus, error) {
	switch state {
	case db.TokenStatusPending:
		return api.TokenStatusPending, nil
	case db.TokenStatusCompleted:
		return api.TokenStatusCompleted, nil
	case db.TokenStatusExpired:
		return api.TokenStatusExpired, nil
	case db.TokenStatusCancelled:
		return api.TokenStatusCancelled, nil
	default:
		return "", fmt.Errorf("token state %q has no public projection", state)
	}
}

// tokenCreateResponse projects a Token as it was created, pending and without
// a result, with its credentials.
func tokenCreateResponse(row db.Token, publicAccessToken string, callbackURL string) (api.TokenResponse, error) {
	creation := row
	creation.Status = db.TokenStatusPending
	creation.Result = nil
	creation.Error = nil
	creation.CompletionFingerprint = nil
	creation.CompletedAt = pgtype.Timestamptz{}
	creation.ExpiredAt = pgtype.Timestamptz{}
	creation.CancelledAt = pgtype.Timestamptz{}
	creation.UpdatedAt = creation.CreatedAt
	response, err := tokenResponse(creation)
	if err != nil {
		return api.TokenResponse{}, err
	}
	response.PublicAccessToken = publicAccessToken
	response.CallbackURL = callbackURL
	return response, nil
}

func (s *Server) writeTokenError(w http.ResponseWriter, err error) {
	writeError(w, tokenError(err))
}

func (s *Server) writeTokenCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Vary", "Origin")
}
