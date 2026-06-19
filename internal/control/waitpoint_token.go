package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicaccess"
	"github.com/helmrdotdev/helmr/internal/waitpoint"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const waitpointTokenCompletionRequestJSONMaxBytes = 1024*1024 + 4096

type waitpointTokenScope struct {
	actor         auth.Actor
	projectID     uuid.UUID
	environmentID uuid.UUID
	scope         auth.Scope
}

type waitpointTokenCreateScope struct {
	orgID         uuid.UUID
	projectID     uuid.UUID
	environmentID uuid.UUID
}

type publicAccessTokenScope struct {
	Type             string `json:"type"`
	WaitpointTokenID string `json:"waitpointTokenId,omitempty"`
	SessionID        string `json:"sessionId,omitempty"`
	Channel          string `json:"channel,omitempty"`
	CorrelationID    string `json:"correlationId,omitempty"`
}

func (s *Server) createWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, unavailable(errors.New("token hashing is not configured")))
		return
	}
	scope, ok := s.authorizeWaitpointTokenScope(w, r, auth.PermissionWaitpointTokensCreate)
	if !ok {
		return
	}
	var request api.CreateWaitpointTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid waitpoint token request JSON: %w", err)))
		return
	}
	token, publicToken, callbackSecret, err := s.createWaitpointTokenRecord(r.Context(), waitpointTokenCreateScope{
		orgID:         scope.actor.OrgID,
		projectID:     scope.projectID,
		environmentID: scope.environmentID,
	}, request)
	if err != nil {
		s.log.Error("create waitpoint token failed", "project_id", scope.projectID.String(), "environment_id", scope.environmentID.String(), "error", err)
		writeWaitpointTokenCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.waitpointTokenResponseFromCreate(token, publicToken, callbackSecret))
}

func (s *Server) workerCreateWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, unavailable(errors.New("token hashing is not configured")))
		return
	}
	var request api.WorkerCreateWaitpointTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker waitpoint token JSON: %w", err)))
		return
	}
	leaseIDs, err := parseWorkerRunLease(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker run lease belongs to another worker")))
		return
	}
	leaseRow, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	} else if err != nil {
		s.log.Error("worker queue lease lookup failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("get queue lease"))
		return
	}
	token, publicToken, callbackSecret, err := s.createWaitpointTokenRecord(r.Context(), waitpointTokenCreateScope{
		orgID:         leaseIDs.orgID,
		projectID:     pgvalue.MustUUIDValue(leaseRow.ProjectID),
		environmentID: pgvalue.MustUUIDValue(leaseRow.EnvironmentID),
	}, request.CreateWaitpointTokenRequest)
	if err != nil {
		s.log.Error("worker create waitpoint token failed", "run_id", request.Lease.RunID, "error", err)
		writeWaitpointTokenCreateError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, s.waitpointTokenResponseFromCreate(token, publicToken, callbackSecret))
}

func (s *Server) createWaitpointTokenRecord(ctx context.Context, scope waitpointTokenCreateScope, request api.CreateWaitpointTokenRequest) (db.WaitpointToken, string, string, error) {
	now := time.Now().UTC()
	timeoutAt, err := waitpointTokenExpiry(now, request.TimeoutAt, request.TimeoutInSeconds, publicaccess.DefaultTokenTTL, "timeout")
	if err != nil {
		return db.WaitpointToken{}, "", "", badRequest(err)
	}
	metadata, err := normalizeWaitpointTokenMetadata(request.Metadata)
	if err != nil {
		return db.WaitpointToken{}, "", "", badRequest(err)
	}
	tags, err := normalizeWaitpointTags(request.Tags)
	if err != nil {
		return db.WaitpointToken{}, "", "", badRequest(err)
	}
	publicToken, publicTokenHash, err := publicaccess.NewToken(s.authSecret)
	if err != nil {
		return db.WaitpointToken{}, "", "", errors.New("generate waitpoint token")
	}
	callbackSecret, callbackSecretHash, err := waitpoint.NewCallbackSecret(s.authSecret)
	if err != nil {
		return db.WaitpointToken{}, "", "", errors.New("generate waitpoint callback secret")
	}
	tokenID := uuid.Must(uuid.NewV7())
	scopes, err := json.Marshal([]publicAccessTokenScope{{
		Type:             "waitpointToken.complete",
		WaitpointTokenID: tokenID.String(),
	}})
	if err != nil {
		return db.WaitpointToken{}, "", "", err
	}
	createdBy := []byte(`{"type":"control"}`)
	maxUses := pgtype.Int4{Int32: 1, Valid: true}
	if s.tx != nil {
		tx, err := s.tx.Begin(ctx)
		if err != nil {
			return db.WaitpointToken{}, "", "", err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		queries := db.New(tx)
		token, err := createWaitpointTokenRows(ctx, queries, scope, tokenID, callbackSecretHash, publicTokenHash, timeoutAt, tags, metadata, scopes, maxUses, createdBy)
		if err != nil {
			return db.WaitpointToken{}, "", "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return db.WaitpointToken{}, "", "", err
		}
		return token, publicToken, callbackSecret, nil
	}
	token, err := createWaitpointTokenRows(ctx, s.db, scope, tokenID, callbackSecretHash, publicTokenHash, timeoutAt, tags, metadata, scopes, maxUses, createdBy)
	if err != nil {
		return db.WaitpointToken{}, "", "", err
	}
	return token, publicToken, callbackSecret, nil
}

type waitpointTokenRowCreator interface {
	CreateWaitpointToken(context.Context, db.CreateWaitpointTokenParams) (db.WaitpointToken, error)
	CreatePublicAccessToken(context.Context, db.CreatePublicAccessTokenParams) (db.PublicAccessToken, error)
}

func createWaitpointTokenRows(ctx context.Context, queries waitpointTokenRowCreator, scope waitpointTokenCreateScope, tokenID uuid.UUID, callbackSecretHash []byte, publicTokenHash []byte, timeoutAt time.Time, tags []string, metadata []byte, scopes []byte, maxUses pgtype.Int4, createdBy []byte) (db.WaitpointToken, error) {
	token, err := queries.CreateWaitpointToken(ctx, db.CreateWaitpointTokenParams{
		OrgID:              pgvalue.UUID(scope.orgID),
		ProjectID:          pgvalue.UUID(scope.projectID),
		EnvironmentID:      pgvalue.UUID(scope.environmentID),
		ID:                 pgvalue.UUID(tokenID),
		CallbackSecretHash: callbackSecretHash,
		TimeoutAt:          pgvalue.Timestamptz(timeoutAt),
		Tags:               tags,
		Metadata:           metadata,
	})
	if err != nil {
		return db.WaitpointToken{}, err
	}
	if _, err := queries.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         pgvalue.UUID(scope.orgID),
		ProjectID:     pgvalue.UUID(scope.projectID),
		EnvironmentID: pgvalue.UUID(scope.environmentID),
		TokenHash:     publicTokenHash,
		AllowedScopes: scopes,
		ExpiresAt:     pgvalue.Timestamptz(timeoutAt),
		MaxUses:       maxUses,
		Metadata:      []byte(`{}`),
		CreatedBy:     createdBy,
	}); err != nil {
		return db.WaitpointToken{}, err
	}
	return token, nil
}

func writeWaitpointTokenCreateError(w http.ResponseWriter, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, err)
		return
	}
	writeError(w, errors.New("create waitpoint token"))
}

func (s *Server) listWaitpointTokens(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	scope, ok := s.authorizeWaitpointTokenScope(w, r, auth.PermissionWaitpointTokensRead)
	if !ok {
		return
	}
	cursorID, err := optionalUUIDQuery(r, "cursor")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := optionalLimitQuery(r, defaultControlPageSize)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	status, err := optionalWaitpointTokenStatusQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	rows, err := s.db.ListWaitpointTokens(r.Context(), db.ListWaitpointTokensParams{
		OrgID:         pgvalue.UUID(scope.actor.OrgID),
		ProjectID:     pgvalue.UUID(scope.projectID),
		EnvironmentID: pgvalue.UUID(scope.environmentID),
		AfterID:       cursorID,
		Status:        status,
		LimitCount:    limit + 1,
	})
	if err != nil {
		s.log.Error("list waitpoint tokens failed", "project_id", scope.projectID.String(), "environment_id", scope.environmentID.String(), "error", err)
		writeError(w, errors.New("list waitpoint tokens"))
		return
	}
	rows, nextCursor := trimWaitpointTokensPage(rows, limit)
	tokens := make([]api.WaitpointTokenResponse, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, waitpointTokenResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListWaitpointTokensResponse{Tokens: tokens, NextCursor: nextCursor})
}

func (s *Server) getWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	scope, ok := s.authorizeWaitpointTokenScope(w, r, auth.PermissionWaitpointTokensRead)
	if !ok {
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, badRequest(errors.New("tokenID must be a UUID")))
		return
	}
	token, err := s.db.GetWaitpointToken(r.Context(), db.GetWaitpointTokenParams{
		OrgID:         pgvalue.UUID(scope.actor.OrgID),
		ProjectID:     pgvalue.UUID(scope.projectID),
		EnvironmentID: pgvalue.UUID(scope.environmentID),
		ID:            pgvalue.UUID(tokenID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("waitpoint token not found")))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, errors.New("get waitpoint token"))
		return
	}
	writeJSON(w, http.StatusOK, waitpointTokenResponse(token))
}

func (s *Server) optionsCompleteWaitpointToken(w http.ResponseWriter, r *http.Request) {
	writeWaitpointTokenCompletionCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeWaitpointToken(w http.ResponseWriter, r *http.Request) {
	writeWaitpointTokenCompletionCORS(w)
	rawToken, ok := bearerToken(r.Header.Get("authorization"))
	if !ok {
		writeError(w, unauthorized(errors.New("missing bearer token")))
		return
	}
	if publicaccess.IsToken(rawToken) {
		s.completeWaitpointTokenWithPublicAccessToken(w, r, rawToken)
		return
	}
	if waitpoint.IsCallbackSecret(rawToken) {
		writeError(w, unauthorized(errors.New("invalid token")))
		return
	}
	actor, err := s.bearerActor(r, rawToken)
	if err != nil {
		writeActorAuthError(w, s.log, err)
		return
	}
	s.completeWaitpointTokenAuthenticated(w, r.WithContext(context.WithValue(r.Context(), actorContextKey{}, actor)))
}

func (s *Server) completeWaitpointTokenAuthenticated(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	actor := actorFromContext(r.Context())
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, badRequest(errors.New("tokenID must be a UUID")))
		return
	}
	limitWaitpointTokenCompletionBody(w, r)
	request, err := decodeCompleteWaitpointTokenRequest(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	token, err := s.db.GetWaitpointTokenForAuthenticatedCompletion(r.Context(), db.GetWaitpointTokenForAuthenticatedCompletionParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(tokenID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("waitpoint token not found")))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, errors.New("complete waitpoint token"))
		return
	}
	scope, err := waitpointTokenAuthScope(actor.OrgID, token)
	if err != nil {
		s.log.Error("waitpoint token has invalid scope", "token_id", tokenID.String(), "error", err)
		writeError(w, errors.New("complete waitpoint token"))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointTokensComplete, scope) {
		writeError(w, notFound(errors.New("waitpoint token not found")))
		return
	}
	s.completeWaitpointTokenRecord(w, r, token, request)
}

func (s *Server) completeWaitpointTokenWithPublicAccessToken(w http.ResponseWriter, r *http.Request, rawToken string) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, unavailable(errors.New("token hashing is not configured")))
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, badRequest(errors.New("tokenID must be a UUID")))
		return
	}
	limitWaitpointTokenCompletionBody(w, r)
	request, err := decodeCompleteWaitpointTokenRequest(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	tokenHash, err := publicaccess.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, unauthorized(errors.New("invalid token")))
		return
	}
	outcome, err := s.completeWaitpointTokenWithPublicAccessTokenTx(r.Context(), tokenID, tokenHash, request)
	if isNoRows(err) {
		writeError(w, notFound(errors.New("invalid or inactive token")))
		return
	}
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr)
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, errors.New("complete waitpoint token"))
		return
	}
	if outcome.Resumed {
		s.enqueueResolvedWaitpointRuns(r.Context(), outcome.OrgID, outcome.RunIDs)
	}
	w.WriteHeader(http.StatusNoContent)
}

func writeWaitpointTokenCompletionCORS(w http.ResponseWriter) {
	header := w.Header()
	header.Set("access-control-allow-origin", "*")
	header.Set("access-control-allow-methods", "POST, OPTIONS")
	header.Set("access-control-allow-headers", strings.Join([]string{
		"authorization",
		"content-type",
		api.APIVersionHeader,
		api.SDKVersionHeader,
	}, ", "))
	header.Set("access-control-max-age", "600")
}

func (s *Server) callbackWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, unavailable(errors.New("token hashing is not configured")))
		return
	}
	tokenID, err := uuid.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, badRequest(errors.New("tokenID must be a UUID")))
		return
	}
	callbackSecret := strings.TrimSpace(chi.URLParam(r, "callbackSecret"))
	callbackSecretHash, err := waitpoint.HashCallbackSecret(s.authSecret, callbackSecret)
	if err != nil {
		writeError(w, unauthorized(errors.New("invalid callback secret")))
		return
	}
	limitWaitpointTokenCompletionBody(w, r)
	data, err := readCallbackData(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	token, err := s.db.GetWaitpointTokenForCallbackCompletion(r.Context(), db.GetWaitpointTokenForCallbackCompletionParams{
		ID:                 pgvalue.UUID(tokenID),
		CallbackSecretHash: callbackSecretHash,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("invalid or inactive token")))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, errors.New("complete waitpoint token"))
		return
	}
	s.completeWaitpointTokenRecord(w, r, token, api.CompleteWaitpointTokenRequest{Data: data})
}

func (s *Server) completeWaitpointTokenRecord(w http.ResponseWriter, r *http.Request, token db.WaitpointToken, request api.CompleteWaitpointTokenRequest) {
	data := request.Data
	if len(data) == 0 {
		data = []byte(`null`)
	}
	if !json.Valid(data) {
		writeError(w, badRequest(errors.New("data must be valid JSON")))
		return
	}
	tokenID := pgvalue.MustUUIDValue(token.ID)
	completionHash, err := waitpointCompletionHash(tokenID, data, waitpointCompletionHashInternalScope())
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("data canonicalization failed: %w", err)))
		return
	}
	outcome, err := s.completeWaitpointTokenTx(r.Context(), db.CompleteWaitpointTokenParams{
		OrgID:          token.OrgID,
		ID:             token.ID,
		Data:           data,
		CompletionHash: pgvalue.Text(completionHash),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("waitpoint token completion conflicts with an existing completion or expired token")))
		return
	}
	if err != nil {
		s.log.Error("complete waitpoint token failed", "token_id", pgvalue.MustUUIDValue(token.ID).String(), "error", err)
		writeError(w, errors.New("complete waitpoint token"))
		return
	}
	if outcome.Resumed {
		s.enqueueResolvedWaitpointRuns(r.Context(), token.OrgID, outcome.RunIDs)
	}
	if outcome.Resumed {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeWaitpointTokenWithPublicAccessTokenTx(ctx context.Context, tokenID uuid.UUID, tokenHash []byte, request api.CompleteWaitpointTokenRequest) (waitpointResolveOutcome, error) {
	data := request.Data
	if len(data) == 0 {
		data = []byte(`null`)
	}
	if !json.Valid(data) {
		return waitpointResolveOutcome{}, badRequest(errors.New("data must be valid JSON"))
	}
	if s.tx == nil {
		return waitpointResolveOutcome{}, unavailable(errors.New("transactional waitpoint token storage is not configured"))
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	publicToken, err := queries.LockPublicAccessTokenByHash(ctx, tokenHash)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if !publicAccessTokenIsActive(publicToken, time.Now()) {
		return waitpointResolveOutcome{}, pgx.ErrNoRows
	}
	scope, ok := publicAccessTokenWaitpointCompletionScope(publicToken.AllowedScopes, tokenID)
	if !ok {
		return waitpointResolveOutcome{}, pgx.ErrNoRows
	}
	completionHash, err := waitpointCompletionHash(tokenID, data, waitpointCompletionHashPublicScope(scope))
	if err != nil {
		return waitpointResolveOutcome{}, badRequest(fmt.Errorf("data canonicalization failed: %w", err))
	}
	token, err := queries.GetWaitpointTokenForPublicCompletion(ctx, db.GetWaitpointTokenForPublicCompletionParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		ID:            pgvalue.UUID(tokenID),
	})
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if token.Status == db.WaitpointTokenStatusCompleted && token.CompletionHash.Valid && token.CompletionHash.String == completionHash {
		// Matching completed retries are acknowledgements of the original
		// authorized action; new or mismatched completions still consume below.
		if err := tx.Commit(ctx); err != nil {
			return waitpointResolveOutcome{}, err
		}
		return waitpointResolveOutcome{OrgID: publicToken.OrgID}, nil
	}
	completedConflict := token.Status == db.WaitpointTokenStatusCompleted
	if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	}); err != nil {
		return waitpointResolveOutcome{}, err
	}
	if completedConflict {
		if err := tx.Commit(ctx); err != nil {
			return waitpointResolveOutcome{}, err
		}
		return waitpointResolveOutcome{}, conflict(errors.New("waitpoint token completion conflicts with an existing completion"))
	}
	row, err := queries.CompleteWaitpointToken(ctx, db.CompleteWaitpointTokenParams{
		OrgID:          publicToken.OrgID,
		ID:             pgvalue.UUID(tokenID),
		Data:           data,
		CompletionHash: pgvalue.Text(completionHash),
	})
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	resumedRows := []db.UnblockRunWaitpointsForWaitpointRow(nil)
	if row.ResolvedWaitpoint && row.WaitpointID.Valid {
		rows, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
			OrgID:       publicToken.OrgID,
			WaitpointID: row.WaitpointID,
		})
		if err != nil {
			return waitpointResolveOutcome{}, err
		}
		resumedRows = rows
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcome{Resumed: len(resumedRows) > 0, OrgID: publicToken.OrgID, RunIDs: waitpointResumeRunIDs(resumedRows)}, nil
}

func publicAccessTokenAllowsWaitpointCompletion(raw []byte, tokenID uuid.UUID) bool {
	_, ok := publicAccessTokenWaitpointCompletionScope(raw, tokenID)
	return ok
}

func publicAccessTokenWaitpointCompletionScope(raw []byte, tokenID uuid.UUID) (publicAccessTokenScope, bool) {
	var scopes []publicAccessTokenScope
	if err := json.Unmarshal(raw, &scopes); err != nil {
		return publicAccessTokenScope{}, false
	}
	for _, scope := range scopes {
		if scope.Type == "waitpointToken.complete" && scope.WaitpointTokenID == tokenID.String() {
			return scope, true
		}
	}
	return publicAccessTokenScope{}, false
}

func (s *Server) completeWaitpointTokenTx(ctx context.Context, params db.CompleteWaitpointTokenParams) (waitpointResolveOutcome, error) {
	if s.tx == nil {
		row, err := s.db.CompleteWaitpointToken(ctx, params)
		if err != nil {
			return waitpointResolveOutcome{}, err
		}
		if !row.ResolvedWaitpoint || !row.WaitpointID.Valid {
			return waitpointResolveOutcome{}, nil
		}
		resumed, err := s.db.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
			OrgID:       params.OrgID,
			WaitpointID: row.WaitpointID,
		})
		if err != nil {
			return waitpointResolveOutcome{}, err
		}
		return waitpointResolveOutcome{Resumed: len(resumed) > 0, RunIDs: waitpointResumeRunIDs(resumed)}, nil
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	row, err := queries.CompleteWaitpointToken(ctx, params)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	resumedRows := []db.UnblockRunWaitpointsForWaitpointRow(nil)
	if row.ResolvedWaitpoint && row.WaitpointID.Valid {
		rows, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
			OrgID:       params.OrgID,
			WaitpointID: row.WaitpointID,
		})
		if err != nil {
			return waitpointResolveOutcome{}, err
		}
		resumedRows = rows
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcome{Resumed: len(resumedRows) > 0, RunIDs: waitpointResumeRunIDs(resumedRows)}, nil
}

func (s *Server) enqueueResolvedWaitpointRuns(ctx context.Context, orgID pgtype.UUID, runIDs []pgtype.UUID) {
	if s.runEnqueuer == nil {
		return
	}
	seen := make(map[string]struct{}, len(runIDs))
	for _, runID := range runIDs {
		if !runID.Valid {
			continue
		}
		key := pgvalue.MustUUIDValue(runID).String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if _, err := s.runEnqueuer.EnqueueRun(ctx, orgID, runID); err != nil {
			s.log.Error("enqueue resumed waitpoint run failed", "run_id", key, "error", err)
		}
	}
}

func waitpointResumeRunIDs(rows []db.UnblockRunWaitpointsForWaitpointRow) []pgtype.UUID {
	runIDs := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		runIDs = append(runIDs, row.RunID)
	}
	return runIDs
}

func (s *Server) authorizeWaitpointTokenScope(w http.ResponseWriter, r *http.Request, permission auth.Permission) (waitpointTokenScope, bool) {
	actor := actorFromContext(r.Context())
	projectID, environmentID, err := s.waitpointTokenRouteScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return waitpointTokenScope{}, false
	}
	projectPG := pgvalue.UUID(projectID)
	environmentPG := pgvalue.UUID(environmentID)
	if err := s.requireActorScopeForRecord(r, actor, projectPG, environmentPG); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("project environment not found")))
			return waitpointTokenScope{}, false
		}
		writeError(w, badRequest(err))
		return waitpointTokenScope{}, false
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return waitpointTokenScope{}, false
	}
	return waitpointTokenScope{
		actor:         actor,
		projectID:     projectID,
		environmentID: environmentID,
		scope:         scope,
	}, true
}

func waitpointTokenAuthScope(orgID uuid.UUID, token db.WaitpointToken) (auth.Scope, error) {
	projectID, err := pgvalue.UUIDValue(token.ProjectID)
	if err != nil {
		return auth.Scope{}, err
	}
	environmentID, err := pgvalue.UUIDValue(token.EnvironmentID)
	if err != nil {
		return auth.Scope{}, err
	}
	return auth.Scope{
		OrgID:         orgID,
		ProjectID:     projectID.String(),
		EnvironmentID: environmentID.String(),
	}, nil
}

func (s *Server) waitpointTokenRouteScope(r *http.Request, actor auth.Actor) (uuid.UUID, uuid.UUID, error) {
	projectParam := strings.TrimSpace(chi.URLParam(r, "projectID"))
	environmentParam := strings.TrimSpace(chi.URLParam(r, "environmentID"))
	if projectParam == "" && environmentParam == "" {
		if actor.ProjectID == "" || actor.EnvironmentID == "" {
			return uuid.Nil, uuid.Nil, errors.New("project and environment are required")
		}
		projectID, err := uuid.Parse(actor.ProjectID)
		if err != nil {
			return uuid.Nil, uuid.Nil, errors.New("actor project scope must be a UUID")
		}
		environmentID, err := uuid.Parse(actor.EnvironmentID)
		if err != nil {
			return uuid.Nil, uuid.Nil, errors.New("actor environment scope must be a UUID")
		}
		return projectID, environmentID, nil
	}
	_, project, environment, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	projectID, err := uuid.FromBytes(project.Bytes[:])
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	environmentID, err := uuid.FromBytes(environment.Bytes[:])
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return projectID, environmentID, nil
}

func decodeCompleteWaitpointTokenRequest(r *http.Request) (api.CompleteWaitpointTokenRequest, error) {
	if strings.Contains(r.Header.Get("content-type"), "application/x-www-form-urlencoded") {
		return api.CompleteWaitpointTokenRequest{}, errors.New("form completion is not supported")
	}
	var request api.CompleteWaitpointTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		return api.CompleteWaitpointTokenRequest{}, fmt.Errorf("invalid waitpoint token completion JSON: %w", err)
	}
	return request, nil
}

func limitWaitpointTokenCompletionBody(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, waitpointTokenCompletionRequestJSONMaxBytes)
}

func readCallbackData(r *http.Request) (json.RawMessage, error) {
	var data json.RawMessage
	if r.Body != nil {
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, fmt.Errorf("read callback body: %w", err)
		}
		data = bytes.TrimSpace(body)
	}
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	if !json.Valid(data) {
		return nil, errors.New("callback body must be valid JSON")
	}
	return data, nil
}

func optionalWaitpointTokenStatusQuery(r *http.Request) (pgtype.Text, error) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		return pgtype.Text{}, nil
	}
	switch status {
	case "waiting", "completed", "timed_out", "cancelled":
		return pgvalue.Text(status), nil
	default:
		return pgtype.Text{}, fmt.Errorf("status must be one of waiting, completed, timed_out, cancelled")
	}
}

func waitpointTokenExpiry(now time.Time, timeoutAt *time.Time, timeoutInSeconds *int32, defaultTTL time.Duration, name string) (time.Time, error) {
	if timeoutAt != nil && timeoutInSeconds != nil {
		return time.Time{}, fmt.Errorf("%s_at and %s_in_seconds cannot both be set", name, name)
	}
	if timeoutAt != nil {
		expiry := timeoutAt.UTC()
		if !expiry.After(now) {
			return time.Time{}, fmt.Errorf("%s_at must be in the future", name)
		}
		return expiry, nil
	}
	if timeoutInSeconds != nil {
		if *timeoutInSeconds <= 0 {
			return time.Time{}, fmt.Errorf("%s_in_seconds must be positive", name)
		}
		return now.Add(time.Duration(*timeoutInSeconds) * time.Second), nil
	}
	return now.Add(defaultTTL), nil
}

func normalizeWaitpointTokenMetadata(metadata json.RawMessage) ([]byte, error) {
	if len(metadata) == 0 {
		return []byte(`{}`), nil
	}
	return normalizeJSONMetadataObject(metadata, "metadata")
}

func waitpointCompletionHash(tokenID uuid.UUID, data json.RawMessage, authScope json.RawMessage) (string, error) {
	if len(data) == 0 {
		data = []byte("null")
	}
	canonical, err := canonicalJSON(data)
	if err != nil {
		return "", err
	}
	envelope, err := json.Marshal(struct {
		TokenID   string          `json:"token_id"`
		Data      json.RawMessage `json:"data"`
		AuthScope json.RawMessage `json:"auth_scope"`
	}{
		TokenID:   tokenID.String(),
		Data:      canonical,
		AuthScope: authScope,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(envelope)
	return hex.EncodeToString(sum[:]), nil
}

func waitpointCompletionHashInternalScope() json.RawMessage {
	return json.RawMessage(`{"type":"internal"}`)
}

func waitpointCompletionHashPublicScope(scope publicAccessTokenScope) json.RawMessage {
	raw, err := json.Marshal(scope)
	if err != nil {
		return json.RawMessage(`{"type":"invalid"}`)
	}
	return raw
}

func (s *Server) waitpointTokenResponseFromCreate(row db.WaitpointToken, publicToken string, callbackSecret string) api.WaitpointTokenResponse {
	response := waitpointTokenCreateRowResponse(row)
	response.PublicAccessToken = publicToken
	response.CallbackURL = waitpoint.CallbackURL(s.publicURL, pgvalue.MustUUIDValue(row.ID).String(), callbackSecret)
	return response
}

func waitpointTokenCreateRowResponse(row db.WaitpointToken) api.WaitpointTokenResponse {
	timeoutAt := pgvalue.Time(row.TimeoutAt)
	var timeoutAtPtr *time.Time
	if row.TimeoutAt.Valid {
		timeoutAtPtr = &timeoutAt
	}
	return api.WaitpointTokenResponse{
		ID:        pgvalue.MustUUIDValue(row.ID).String(),
		Status:    string(row.Status),
		TimeoutAt: timeoutAtPtr,
		Data:      row.Data,
		Tags:      row.Tags,
		Metadata:  row.Metadata,
	}
}

func waitpointTokenResponse(row db.WaitpointToken) api.WaitpointTokenResponse {
	timeoutAt := pgvalue.Time(row.TimeoutAt)
	var timeoutAtPtr *time.Time
	if row.TimeoutAt.Valid {
		timeoutAtPtr = &timeoutAt
	}
	return api.WaitpointTokenResponse{
		ID:        pgvalue.MustUUIDValue(row.ID).String(),
		Status:    string(row.Status),
		TimeoutAt: timeoutAtPtr,
		Data:      row.Data,
		Tags:      row.Tags,
		Metadata:  row.Metadata,
	}
}

func trimWaitpointTokensPage(rows []db.WaitpointToken, limit int32) ([]db.WaitpointToken, *string) {
	if int32(len(rows)) <= limit {
		return rows, nil
	}
	page := rows[:limit]
	next := pgvalue.MustUUIDValue(page[len(page)-1].ID).String()
	return page, &next
}
