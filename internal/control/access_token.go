package control

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	errTokenNotFound           = codedError{code: "token_not_found", message: "token not found"}
	errTokenExpired            = codedError{code: "token_expired", message: "token expired"}
	errTokenCancelled          = codedError{code: "token_cancelled", message: "token cancelled"}
	errTokenScopeDenied        = codedError{code: "token_scope_denied", message: "token scope denied"}
	errTokenCompletionConflict = codedError{code: "token_completion_conflict", message: "token completion conflicts with existing completion"}
	errStreamNotFound          = codedError{code: "stream_not_found", message: "stream not found"}
	errStreamDirectionMismatch = codedError{code: "stream_direction_mismatch", message: "stream direction mismatch"}
	errIdempotencyFingerprint  = codedError{code: "idempotency_fingerprint_mismatch", message: "idempotency key was already used with different request parameters"}
)

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var request api.CreateTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token create request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, request.ProjectID, request.EnvironmentID)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTokensCreate, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	var token db.CreateTokenRow
	var publicToken string
	err = s.inTx(r.Context(), func(work *txWork) error {
		var err error
		token, publicToken, err = s.createTokenRecord(r.Context(), work.q, actor, projectID, environmentID, request)
		return err
	})
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	status := http.StatusCreated
	if token.IsCached {
		status = http.StatusOK
	}
	writeJSON(w, status, tokenResponse(tokenFromCreateRow(token), publicToken, s.tokenCallbackURL(pgvalue.MustUUIDValue(token.ID))))
}

func (s *Server) createTokenRecord(ctx context.Context, store db.Querier, actor auth.Actor, projectID pgtype.UUID, environmentID pgtype.UUID, request api.CreateTokenRequest) (db.CreateTokenRow, string, error) {
	timeoutAt, err := tokenTimeoutAt(request.Timeout)
	if err != nil {
		return db.CreateTokenRow{}, "", badRequest(err)
	}
	metadata, err := normalizedJSONObject(request.Metadata, "metadata")
	if err != nil {
		return db.CreateTokenRow{}, "", badRequest(err)
	}
	tags, err := normalizedRunTags(request.Tags)
	if err != nil {
		return db.CreateTokenRow{}, "", badRequest(err)
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return db.CreateTokenRow{}, "", badRequest(err)
	}
	tokenID := uuid.Must(uuid.NewV7())
	_, callbackFingerprint, err := s.deterministicTokenSecret("callback", tokenID)
	if err != nil {
		return db.CreateTokenRow{}, "", err
	}
	publicToken, publicTokenHash, err := s.deterministicTokenSecret("public-access", tokenID)
	if err != nil {
		return db.CreateTokenRow{}, "", err
	}
	fingerprint, err := tokenCreateFingerprint(request.Timeout, metadata, tags)
	if err != nil {
		return db.CreateTokenRow{}, "", err
	}
	row, err := store.CreateToken(ctx, db.CreateTokenParams{
		ID:                        pgvalue.UUID(tokenID),
		OrgID:                     pgvalue.UUID(actor.OrgID),
		ProjectID:                 projectID,
		EnvironmentID:             environmentID,
		TimeoutAt:                 pgvalue.Timestamptz(timeoutAt),
		IdempotencyKey:            idempotencyKey,
		CreateRequestFingerprint:  fingerprint,
		CallbackKeyID:             tokenCallbackKeyID,
		CallbackSecretFingerprint: hex.EncodeToString(callbackFingerprint),
		CallbackSecretCreatedAt:   pgvalue.Timestamptz(time.Now()),
		Metadata:                  metadata,
		Tags:                      tags,
	})
	if err != nil {
		return db.CreateTokenRow{}, "", err
	}
	if row.IdempotencyFingerprintMismatch {
		return row, "", conflict(errIdempotencyFingerprint)
	}
	if row.IsCached {
		existingID := pgvalue.MustUUIDValue(row.ID)
		publicToken, _, err = s.deterministicTokenSecret("public-access", existingID)
		if err != nil {
			return db.CreateTokenRow{}, "", err
		}
		return row, publicToken, nil
	}
	publicAccessToken, err := store.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		TokenHash:     publicTokenHash,
		ExpiresAt:     pgvalue.Timestamptz(timeoutAt.Add(publicAccessTokenTTL)),
		MaxUses:       pgtype.Int4{Int32: 1, Valid: true},
		Metadata:      []byte(`{}`),
		CreatedBy:     []byte(`{"kind":"token.create"}`),
	})
	if err != nil {
		return db.CreateTokenRow{}, "", err
	}
	if _, err := store.CreatePublicAccessTokenScope(ctx, db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               pgvalue.UUID(actor.OrgID),
		ProjectID:           projectID,
		EnvironmentID:       environmentID,
		PublicAccessTokenID: publicAccessToken.ID,
		ScopeType:           db.PublicAccessTokenScopeTypeTokencomplete,
		TokenID:             row.ID,
	}); err != nil {
		return db.CreateTokenRow{}, "", err
	}
	return row, publicToken, nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	projectID, environmentID, err := runScopeIDs(scope)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTokensRead, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return
	}
	limit, err := optionalLimitQuery(r, defaultTokenListLimit)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	afterID := pgtype.UUID{}
	if raw := strings.TrimSpace(firstNonEmptyString(r.URL.Query().Get("after"), r.URL.Query().Get("cursor"))); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, badRequest(errors.New("after must be a token UUID")))
			return
		}
		afterID = pgvalue.UUID(parsed)
	}
	state := pgtype.Text{}
	if raw := strings.TrimSpace(firstNonEmptyString(r.URL.Query().Get("state"), r.URL.Query().Get("status"))); raw != "" {
		switch db.TokenState(raw) {
		case db.TokenStatePending, db.TokenStateCompleted, db.TokenStateExpired, db.TokenStateCancelled:
			state = pgtype.Text{String: raw, Valid: true}
		default:
			writeError(w, badRequest(errors.New("state must be pending, completed, expired, or cancelled")))
			return
		}
	}
	rows, err := s.db.ListTokens(r.Context(), db.ListTokensParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		State:         state,
		AfterID:       afterID,
		LimitCount:    limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list tokens"))
		return
	}
	var nextCursor *string
	if len(rows) > int(limit) {
		cursor := pgvalue.MustUUIDValue(rows[limit-1].ID).String()
		nextCursor = &cursor
		rows = rows[:limit]
	}
	out := make([]api.TokenResponse, 0, len(rows))
	for _, row := range rows {
		out = append(out, tokenResponse(row, "", ""))
	}
	writeJSON(w, http.StatusOK, api.ListTokensResponse{Tokens: out, NextCursor: nextCursor})
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	token, ok := s.authorizeTokenRecord(w, r, actor, auth.PermissionTokensRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse(token, "", ""))
}

func (s *Server) completeToken(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token complete request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	token, ok := s.authorizeTokenRecord(w, r, actor, auth.PermissionTokensComplete)
	if !ok {
		return
	}
	completed, err := s.completeTokenRecord(r.Context(), s.db, token, request.Data)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	s.requeueResolvedRunWaits(r.Context(), token.OrgID)
	writeJSON(w, http.StatusOK, tokenCompleteResponse(completed))
}

func (s *Server) cancelToken(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	token, ok := s.authorizeTokenRecord(w, r, actor, auth.PermissionTokensCancel)
	if !ok {
		return
	}
	cancelled, err := s.db.CancelToken(r.Context(), db.CancelTokenParams{
		OrgID:         token.OrgID,
		ProjectID:     token.ProjectID,
		EnvironmentID: token.EnvironmentID,
		ID:            token.ID,
	})
	if isNoRows(err) {
		if token.State == db.TokenStateCancelled {
			writeError(w, conflict(errTokenCancelled))
			return
		}
		if token.State == db.TokenStateExpired || (token.State == db.TokenStatePending && pgvalue.Time(token.TimeoutAt).Before(time.Now())) {
			writeError(w, gone(errTokenExpired))
			return
		}
		writeError(w, notFound(errTokenNotFound))
		return
	}
	if err != nil {
		writeError(w, errors.New("cancel token"))
		return
	}
	if cancelled.ResolvedWaitCount > 0 {
		if _, err := s.db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(r.Context(), db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
			OrgID:      token.OrgID,
			LimitCount: int32(cancelled.ResolvedWaitCount),
		}); err != nil {
			writeError(w, errors.New("publish hot token cancellation"))
			return
		}
	}
	s.requeueResolvedRunWaits(r.Context(), token.OrgID)
	writeJSON(w, http.StatusOK, tokenResponse(tokenFromCancelRow(cancelled), "", ""))
}

func (s *Server) completeTokenWithCallbackSecret(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token callback request JSON: %w", err)))
		return
	}
	tokenID, err := parseUUIDParam(r, "tokenID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	callbackSecret := strings.TrimSpace(chi.URLParam(r, "callbackSecret"))
	if callbackSecret == "" {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	fingerprint, err := auth.HashToken(s.authSecret, callbackSecret)
	if err != nil {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	token, err := s.db.GetTokenForCallbackCompletion(r.Context(), db.GetTokenForCallbackCompletionParams{
		ID:                        pgvalue.UUID(tokenID),
		CallbackKeyID:             tokenCallbackKeyID,
		CallbackSecretFingerprint: hex.EncodeToString(fingerprint),
	})
	if isNoRows(err) {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	if err != nil {
		writeError(w, errors.New("authorize token callback"))
		return
	}
	completed, err := s.completeTokenRecord(r.Context(), s.db, token, request.Data)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	s.requeueResolvedRunWaits(r.Context(), token.OrgID)
	writeJSON(w, http.StatusOK, tokenCompleteResponse(completed))
}

func (s *Server) authorizeTokenRecord(w http.ResponseWriter, r *http.Request, actor auth.Actor, permission auth.Permission) (db.Token, bool) {
	tokenID, err := parseUUIDParam(r, "tokenID")
	if err != nil {
		writeError(w, badRequest(err))
		return db.Token{}, false
	}
	token, err := s.db.GetTokenByID(r.Context(), pgvalue.UUID(tokenID))
	if isNoRows(err) {
		writeError(w, notFound(errTokenNotFound))
		return db.Token{}, false
	}
	if err != nil {
		writeError(w, errors.New("load token"))
		return db.Token{}, false
	}
	if err := s.requireActorScopeForRecord(r, actor, token.ProjectID, token.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errTokenNotFound))
			return db.Token{}, false
		}
		writeError(w, badRequest(err))
		return db.Token{}, false
	}
	scope := auth.Scope{OrgID: actor.OrgID, ProjectID: pgvalue.MustUUIDValue(token.ProjectID).String(), EnvironmentID: pgvalue.MustUUIDValue(token.EnvironmentID).String()}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.Token{}, false
	}
	return token, true
}

func (s *Server) completeTokenRecord(ctx context.Context, store db.Querier, token db.Token, data json.RawMessage) (db.CompleteTokenRow, error) {
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	if !json.Valid(data) {
		return db.CompleteTokenRow{}, badRequest(errors.New("data must be valid JSON"))
	}
	if token.State == db.TokenStateCancelled {
		return db.CompleteTokenRow{}, conflict(errTokenCancelled)
	}
	if token.State == db.TokenStateExpired || (token.State == db.TokenStatePending && pgvalue.Time(token.TimeoutAt).Before(time.Now())) {
		return db.CompleteTokenRow{}, gone(errTokenExpired)
	}
	fingerprint, err := jsonFingerprint(data)
	if err != nil {
		return db.CompleteTokenRow{}, err
	}
	completed, err := store.CompleteToken(ctx, db.CompleteTokenParams{
		OrgID:                 token.OrgID,
		ProjectID:             token.ProjectID,
		EnvironmentID:         token.EnvironmentID,
		ID:                    token.ID,
		CompletionData:        data,
		CompletionContentType: "application/json",
		CompletionFingerprint: fingerprint,
	})
	if isNoRows(err) {
		return db.CompleteTokenRow{}, notFound(errTokenNotFound)
	}
	if err != nil {
		return db.CompleteTokenRow{}, err
	}
	if completed.CompletionExpired {
		return completed, gone(errTokenExpired)
	}
	if completed.CompletionConflict {
		return completed, conflict(errTokenCompletionConflict)
	}
	if completed.ResolvedWaitCount > 0 {
		if _, err := store.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(ctx, db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
			OrgID:      token.OrgID,
			LimitCount: int32(completed.ResolvedWaitCount),
		}); err != nil {
			return db.CompleteTokenRow{}, err
		}
	}
	return completed, nil
}

func tokenResponse(row db.Token, publicToken string, callbackURL string) api.TokenResponse {
	var timeoutAt *time.Time
	if row.TimeoutAt.Valid {
		value := row.TimeoutAt.Time
		timeoutAt = &value
	}
	response := api.TokenResponse{
		ID:          pgvalue.MustUUIDValue(row.ID).String(),
		Status:      string(row.State),
		CallbackURL: callbackURL,
		TimeoutAt:   timeoutAt,
		Tags:        row.Tags,
		Metadata:    json.RawMessage(row.Metadata),
	}
	if publicToken != "" {
		response.PublicAccessToken = publicToken
	}
	if len(row.CompletionData) > 0 {
		response.Data = json.RawMessage(row.CompletionData)
	}
	return response
}

func tokenCompleteResponse(row db.CompleteTokenRow) api.CompleteTokenResponse {
	status := "completed"
	if row.AlreadyCompleted {
		status = "already_completed"
	}
	return api.CompleteTokenResponse{
		Status: status,
		Token:  tokenResponse(tokenFromCompleteRow(row), "", ""),
	}
}

func tokenFromCreateRow(row db.CreateTokenRow) db.Token {
	return db.Token{
		ID:                        row.ID,
		OrgID:                     row.OrgID,
		ProjectID:                 row.ProjectID,
		EnvironmentID:             row.EnvironmentID,
		State:                     row.State,
		TimeoutAt:                 row.TimeoutAt,
		IdempotencyKey:            row.IdempotencyKey,
		IdempotencyKeyExpiresAt:   row.IdempotencyKeyExpiresAt,
		CreateRequestFingerprint:  row.CreateRequestFingerprint,
		CallbackKeyID:             row.CallbackKeyID,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CallbackSecretCreatedAt:   row.CallbackSecretCreatedAt,
		CompletionFingerprint:     row.CompletionFingerprint,
		CompletionData:            row.CompletionData,
		CompletionContentType:     row.CompletionContentType,
		Metadata:                  row.Metadata,
		Tags:                      row.Tags,
		CreatedAt:                 row.CreatedAt,
		UpdatedAt:                 row.UpdatedAt,
		CompletedAt:               row.CompletedAt,
		ExpiredAt:                 row.ExpiredAt,
		CancelledAt:               row.CancelledAt,
	}
}

func tokenFromCompleteRow(row db.CompleteTokenRow) db.Token {
	return db.Token{
		ID:                        row.ID,
		OrgID:                     row.OrgID,
		ProjectID:                 row.ProjectID,
		EnvironmentID:             row.EnvironmentID,
		State:                     row.State,
		TimeoutAt:                 row.TimeoutAt,
		IdempotencyKey:            row.IdempotencyKey,
		IdempotencyKeyExpiresAt:   row.IdempotencyKeyExpiresAt,
		CreateRequestFingerprint:  row.CreateRequestFingerprint,
		CallbackKeyID:             row.CallbackKeyID,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CallbackSecretCreatedAt:   row.CallbackSecretCreatedAt,
		CompletionFingerprint:     row.CompletionFingerprint,
		CompletionData:            row.CompletionData,
		CompletionContentType:     row.CompletionContentType,
		Metadata:                  row.Metadata,
		Tags:                      row.Tags,
		CreatedAt:                 row.CreatedAt,
		UpdatedAt:                 row.UpdatedAt,
		CompletedAt:               row.CompletedAt,
		ExpiredAt:                 row.ExpiredAt,
		CancelledAt:               row.CancelledAt,
	}
}

func tokenFromCancelRow(row db.CancelTokenRow) db.Token {
	return db.Token{
		ID:                        row.ID,
		OrgID:                     row.OrgID,
		ProjectID:                 row.ProjectID,
		EnvironmentID:             row.EnvironmentID,
		State:                     row.State,
		TimeoutAt:                 row.TimeoutAt,
		IdempotencyKey:            row.IdempotencyKey,
		IdempotencyKeyExpiresAt:   row.IdempotencyKeyExpiresAt,
		CreateRequestFingerprint:  row.CreateRequestFingerprint,
		CallbackKeyID:             row.CallbackKeyID,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CallbackSecretCreatedAt:   row.CallbackSecretCreatedAt,
		CompletionFingerprint:     row.CompletionFingerprint,
		CompletionData:            row.CompletionData,
		CompletionContentType:     row.CompletionContentType,
		Metadata:                  row.Metadata,
		Tags:                      row.Tags,
		CreatedAt:                 row.CreatedAt,
		UpdatedAt:                 row.UpdatedAt,
		CompletedAt:               row.CompletedAt,
		ExpiredAt:                 row.ExpiredAt,
		CancelledAt:               row.CancelledAt,
	}
}

func (s *Server) tokenCallbackURL(tokenID uuid.UUID) string {
	callbackSecret, _, err := s.deterministicTokenSecret("callback", tokenID)
	if err != nil || s.publicURL == nil {
		return ""
	}
	return s.publicURL.ResolveReference(&url.URL{Path: "/api/v1/tokens/" + tokenID.String() + "/callback/" + callbackSecret}).String()
}

func (s *Server) deterministicTokenSecret(kind string, tokenID uuid.UUID) (string, []byte, error) {
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		return "", nil, err
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write([]byte("helmr:" + kind + ":" + tokenCallbackKeyID + ":" + tokenID.String()))
	raw := "hlmr_" + strings.ReplaceAll(kind, "-", "_") + "_" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	fingerprint, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return "", nil, err
	}
	return raw, fingerprint, nil
}

func tokenTimeoutAt(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return time.Now().Add(defaultTokenTimeout), nil
	}
	duration, err := parseDurationInput(raw, "timeout")
	if err != nil {
		return time.Time{}, err
	}
	return time.Now().Add(duration), nil
}

func parseDurationInput(raw json.RawMessage, label string) (time.Duration, error) {
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return api.ParsePositiveDuration(asString, label)
	}
	var asNumber float64
	if err := json.Unmarshal(raw, &asNumber); err == nil {
		if asNumber <= 0 {
			return 0, fmt.Errorf("%s must be positive", label)
		}
		return time.Duration(asNumber * float64(time.Millisecond)), nil
	}
	var object struct {
		Milliseconds float64 `json:"milliseconds,omitempty"`
		Seconds      float64 `json:"seconds,omitempty"`
		Minutes      float64 `json:"minutes,omitempty"`
		Hours        float64 `json:"hours,omitempty"`
		Duration     string  `json:"duration,omitempty"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return 0, fmt.Errorf("%s must be a duration string, number, or object", label)
	}
	var total time.Duration
	if strings.TrimSpace(object.Duration) != "" {
		duration, err := api.ParsePositiveDuration(object.Duration, label)
		if err != nil {
			return 0, err
		}
		total += duration
	}
	total += time.Duration(object.Milliseconds * float64(time.Millisecond))
	total += time.Duration(object.Seconds * float64(time.Second))
	total += time.Duration(object.Minutes * float64(time.Minute))
	total += time.Duration(object.Hours * float64(time.Hour))
	if total <= 0 {
		return 0, fmt.Errorf("%s must be positive", label)
	}
	return total, nil
}

func tokenCreateFingerprint(timeout json.RawMessage, metadata []byte, tags []string) (string, error) {
	var timeoutValue any
	if len(timeout) == 0 {
		timeoutValue = nil
	} else {
		decoder := json.NewDecoder(bytes.NewReader(timeout))
		decoder.UseNumber()
		if err := decoder.Decode(&timeoutValue); err != nil {
			return "", err
		}
	}
	payload, err := json.Marshal(map[string]any{
		"timeout":  timeoutValue,
		"metadata": json.RawMessage(metadata),
		"tags":     tags,
	})
	if err != nil {
		return "", err
	}
	return jsonFingerprint(payload)
}

func (s *Server) writeStreamTokenError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTokenNotFound) || errors.Is(err, errStreamNotFound) {
		writeError(w, notFound(err))
		return
	}
	writeError(w, err)
}
