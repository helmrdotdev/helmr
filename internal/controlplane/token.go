package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
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
	"github.com/helmrdotdev/helmr/internal/idempotency"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultTokenListLimit = int32(100)
	defaultTokenTimeout   = 10 * time.Minute
	maxTokenTimeout       = 365 * 24 * time.Hour
	tokenRequestBodyLimit = int64(1 << 20)
)

var (
	errTokenNotFound           = codedError{code: "token_not_found", message: "Token was not found"}
	errTokenExpired            = codedError{code: "token_expired", message: "Token has expired"}
	errTokenCancelled          = codedError{code: "token_cancelled", message: "Token was cancelled"}
	errTokenCompleted          = codedError{code: "token_completed", message: "Token is already completed"}
	errTokenScopeDenied        = codedError{code: "token_scope_denied", message: "Token credential is invalid"}
	errTokenCompletionConflict = codedError{code: "token_completion_conflict", message: "Token completion conflicts with the existing result"}
	errTokenCreateReceipt      = errors.New("Token create receipt is invalid")
	errTokenOperationReceipt   = errors.New("Token operation receipt is invalid")
	errTokenCreateAuthority    = errors.New("Token create source authority is stale")
)

type tokenCreateReceipt struct {
	TokenID string `json:"token_id"`
}

type tokenOperationReceipt struct {
	TokenID string `json:"token_id"`
	Outcome string `json:"outcome"`
}

type runtimeTokenCreate struct {
	Lease          workerapi.RunLeaseFence
	ParsedLease    parsedRunLeaseFence
	Worker         workerActor
	CorrelationID  uuid.UUID
	TimeoutMS      *int64
	Metadata       json.RawMessage
	Tags           []string
	IdempotencyKey string
}

type tokenCreateInput struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	TimeoutMS     int64
	Metadata      json.RawMessage
	Tags          []string
	CreatedBy     json.RawMessage
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var request api.CreateTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token create JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTokensCreate, scope) {
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
		writeError(w, badRequest(fmt.Errorf("invalid Token annotations: %w", err)))
		return
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	response, replayed, err := s.createExternalToken(
		r.Context(),
		tokenCreateInput{
			OrgID: pgvalue.UUID(actor.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
			TimeoutMS: timeoutMS, Metadata: metadata, Tags: tags,
			CreatedBy: json.RawMessage(`{"kind":"external"}`),
		},
		idempotencyKey,
	)
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}

func (s *Server) createExternalToken(
	ctx context.Context,
	input tokenCreateInput,
	idempotencyKey string,
) (api.TokenResponse, bool, error) {
	var response api.TokenResponse
	var replayed bool
	err := s.inTx(ctx, func(work *txWork) error {
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if idempotencyKey != "" {
			request, err := idempotency.NewExternalTokenCreateRequest(
				pgvalue.MustUUIDValue(input.EnvironmentID),
				idempotencyKey,
				idempotency.TokenCreateFingerprint{
					TimeoutMS: &input.TimeoutMS,
					Metadata:  input.Metadata,
					Tags:      input.Tags,
				},
			)
			if err != nil {
				return err
			}
			claims, err = idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, request)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				response, err = s.replayTokenCreate(ctx, work.q, acquired.Claim.Receipt)
				replayed = true
				return err
			}
			if acquired.Claim.State != "pending" {
				return errTokenCreateReceipt
			}
			claim = &acquired.Claim
		}
		created, receipt, err := s.createTokenInTransaction(ctx, work.q, input)
		if err != nil {
			return err
		}
		if claim != nil {
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		response = created
		return nil
	})
	return response, replayed, err
}

func (s *Server) workerCreateToken(w http.ResponseWriter, r *http.Request) {
	var request workerapi.CreateTokenRequest
	if err := decodeClosedWorkerRequest(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker Token create JSON: %w", err)))
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
		writeError(w, badRequest(fmt.Errorf("invalid Token annotations: %w", err)))
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
	response, replayed, err := s.createRuntimeToken(r.Context(), runtimeTokenCreate{
		Lease: request.Lease, ParsedLease: parsed, Worker: worker,
		CorrelationID: correlationID, TimeoutMS: timeoutMS,
		Metadata: metadata, Tags: tags, IdempotencyKey: idempotencyKey,
	})
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, response)
}

func (s *Server) createRuntimeToken(
	ctx context.Context,
	request runtimeTokenCreate,
) (api.TokenResponse, bool, error) {
	var response api.TokenResponse
	var replayed bool
	err := s.inTx(ctx, func(work *txWork) error {
		locators, err := loadTokenCreateLocators(
			ctx, work.q, request.Worker, request.Lease, request.ParsedLease,
		)
		if err != nil {
			return err
		}
		environmentUUID := pgvalue.MustUUIDValue(locators.EnvironmentID)
		claimRequest, err := idempotency.NewRuntimeTokenCreateRequest(
			environmentUUID,
			pgvalue.MustUUIDValue(locators.RunID),
			request.IdempotencyKey,
			idempotency.TokenCreateFingerprint{
				TimeoutMS: request.TimeoutMS,
				Metadata:  request.Metadata,
				Tags:      request.Tags,
			},
		)
		if err != nil {
			return err
		}
		claims, err := idempotency.TransactionForQueries(work.q)
		if err != nil {
			return err
		}
		acquired, err := claims.Acquire(ctx, claimRequest)
		if err != nil {
			return err
		}
		locators, _, err = lockTokenCreateAuthority(
			ctx, work.q, request.Worker, request.Lease, request.ParsedLease,
		)
		if err != nil {
			return err
		}
		if acquired.Claim.State == "completed" {
			response, err = s.replayTokenCreate(ctx, work.q, acquired.Claim.Receipt)
			replayed = true
			return err
		}
		if acquired.Claim.State != "pending" {
			return errTokenCreateReceipt
		}

		created, receipt, err := s.createTokenInTransaction(ctx, work.q, tokenCreateInput{
			OrgID: locators.OrgID, ProjectID: locators.ProjectID,
			EnvironmentID: locators.EnvironmentID, TimeoutMS: *request.TimeoutMS,
			Metadata: request.Metadata, Tags: request.Tags,
			CreatedBy: json.RawMessage(`{"kind":"runtime"}`),
		})
		if err != nil {
			return err
		}
		response = created
		if _, err := claims.Complete(ctx, acquired.Claim, receipt); err != nil {
			return err
		}
		return nil
	})
	return response, replayed, err
}

func (s *Server) createTokenInTransaction(
	ctx context.Context,
	q db.Querier,
	input tokenCreateInput,
) (api.TokenResponse, json.RawMessage, error) {
	now, err := q.GetTokenCreateTime(ctx)
	if err != nil || !now.Valid {
		return api.TokenResponse{}, nil, errors.New("load Token create time")
	}
	expiresAt := pgvalue.Timestamptz(
		now.Time.Add(time.Duration(input.TimeoutMS) * time.Millisecond),
	)
	tokenID := uuid.Must(uuid.NewV7())
	credentials, err := s.tokenCredentialKey.Derive(tokenID)
	if err != nil {
		return api.TokenResponse{}, nil, err
	}
	tokenRow, err := q.CreateToken(ctx, db.CreateTokenParams{
		ID:    pgvalue.UUID(tokenID),
		OrgID: input.OrgID, ProjectID: input.ProjectID,
		EnvironmentID: input.EnvironmentID, ExpiresAt: expiresAt,
		CallbackSecretFingerprint: credentials.CallbackFingerprint,
		Metadata:                  input.Metadata, Tags: input.Tags,
	})
	if err != nil {
		return api.TokenResponse{}, nil, fmt.Errorf("create Token: %w", err)
	}
	publicAccessID := uuid.Must(uuid.NewV7())
	_, err = q.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:        pgvalue.UUID(publicAccessID),
		TokenID:   tokenRow.ID,
		TokenHash: credentials.PublicAccessHash,
		ExpiresAt: expiresAt, Metadata: []byte(`{}`),
		CreatedBy: input.CreatedBy,
	})
	if err != nil {
		return api.TokenResponse{}, nil, fmt.Errorf("create Token public access credential: %w", err)
	}
	receipt, err := json.Marshal(tokenCreateReceipt{TokenID: tokenID.String()})
	if err != nil {
		return api.TokenResponse{}, nil, err
	}
	return s.tokenCreateResponse(tokenRow, credentials), receipt, nil
}

func loadTokenCreateLocators(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
) (db.GetLiveRunLeaseLocatorsRow, error) {
	locators, err := q.GetLiveRunLeaseLocators(ctx, db.GetLiveRunLeaseLocatorsParams{
		ID: pgvalue.UUID(parsed.leaseID), LeaseSequence: lease.LeaseSequence,
		WorkerGroupID:    worker.WorkerGroupID,
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		WorkerEpoch:      worker.WorkerEpoch, WorkerProtocolVersion: worker.ProtocolVersion,
	})
	if err != nil {
		return db.GetLiveRunLeaseLocatorsRow{}, errTokenCreateAuthority
	}
	return locators, nil
}

func lockTokenCreateAuthority(
	ctx context.Context,
	q db.Querier,
	worker workerActor,
	lease workerapi.RunLeaseFence,
	parsed parsedRunLeaseFence,
) (db.GetLiveRunLeaseLocatorsRow, runLeaseClaimAuthority, error) {
	locators, err := loadTokenCreateLocators(ctx, q, worker, lease, parsed)
	if err != nil {
		return db.GetLiveRunLeaseLocatorsRow{}, runLeaseClaimAuthority{}, errTokenCreateAuthority
	}
	authority, err := lockLiveRunLeaseAuthority(
		ctx, q, worker, pgvalue.UUID(parsed.leaseID), lease.LeaseSequence, locators,
	)
	if err != nil ||
		authority.run.Status != db.RunStatusRunning ||
		authority.runLease.State != db.RunLeaseStateRunning ||
		!authority.run.ActiveStartedAt.Valid ||
		!authority.attempt.EntrypointEnteredAt.Valid ||
		authority.attempt.TerminalAt.Valid ||
		authority.runLease.FinalizationOperationID.Valid {
		return db.GetLiveRunLeaseLocatorsRow{}, runLeaseClaimAuthority{}, errTokenCreateAuthority
	}
	return locators, authority, nil
}

func (s *Server) replayTokenCreate(
	ctx context.Context,
	q db.Querier,
	rawReceipt []byte,
) (api.TokenResponse, error) {
	var receipt tokenCreateReceipt
	if err := decodeClosedJSON(rawReceipt, &receipt); err != nil {
		return api.TokenResponse{}, errTokenCreateReceipt
	}
	tokenID, err := ids.Parse(receipt.TokenID)
	if err != nil {
		return api.TokenResponse{}, errTokenCreateReceipt
	}
	tokenRow, err := q.GetTokenByID(ctx, pgvalue.UUID(tokenID))
	if err != nil {
		return api.TokenResponse{}, errTokenCreateReceipt
	}
	publicAccess, err := q.GetPublicAccessTokenForToken(ctx, pgvalue.UUID(tokenID))
	if err != nil {
		return api.TokenResponse{}, errTokenCreateReceipt
	}
	credentials, err := s.tokenCredentialKey.Derive(tokenID)
	if err != nil {
		return api.TokenResponse{}, err
	}
	if !hmac.Equal(credentials.CallbackFingerprint, tokenRow.CallbackSecretFingerprint) ||
		!hmac.Equal(credentials.PublicAccessHash, publicAccess.TokenHash) {
		return api.TokenResponse{}, errTokenCreateReceipt
	}
	return s.tokenCreateResponse(tokenRow, credentials), nil
}

func normalizeTokenTimeout(raw *int64) (*int64, error) {
	if raw == nil {
		value := defaultTokenTimeout.Milliseconds()
		return &value, nil
	}
	if *raw < 1 || *raw > maxTokenTimeout.Milliseconds() {
		return nil, fmt.Errorf("timeout_ms must be between 1 and %d", maxTokenTimeout.Milliseconds())
	}
	value := *raw
	return &value, nil
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	actor := actorFromContext(r.Context())
	scope, err := s.requestedRunListScope(r, actor)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !actor.HasPermission(auth.PermissionTokensRead, scope) {
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
	afterID := pgtype.UUID{}
	if raw := strings.TrimSpace(firstNonEmptyString(r.URL.Query().Get("after"), r.URL.Query().Get("cursor"))); raw != "" {
		cursor, err := ids.Parse(raw)
		if err != nil {
			writeError(w, badRequest(errors.New("cursor must be a Token UUID")))
			return
		}
		afterID = pgvalue.UUID(cursor)
	}
	state := pgtype.Text{}
	if raw := strings.TrimSpace(firstNonEmptyString(r.URL.Query().Get("state"), r.URL.Query().Get("status"))); raw != "" {
		switch db.TokenState(raw) {
		case db.TokenStatePending, db.TokenStateCompleted, db.TokenStateExpired, db.TokenStateCancelled:
			state = pgvalue.Text(raw)
		default:
			writeError(w, badRequest(errors.New("status must be pending, completed, expired, or cancelled")))
			return
		}
	}
	rows, err := s.db.ListTokens(r.Context(), db.ListTokensParams{
		OrgID: pgvalue.UUID(actor.OrgID), ProjectID: projectID, EnvironmentID: environmentID,
		State: state, AfterID: afterID, LimitCount: limit + 1,
	})
	if err != nil {
		writeError(w, errors.New("list Tokens"))
		return
	}
	var nextCursor *string
	if len(rows) > int(limit) {
		cursor := pgvalue.MustUUIDValue(rows[limit-1].ID).String()
		nextCursor = &cursor
		rows = rows[:limit]
	}
	tokens := make([]api.TokenResponse, 0, len(rows))
	for _, row := range rows {
		tokens = append(tokens, tokenResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListTokensResponse{Tokens: tokens, NextCursor: nextCursor})
}

func (s *Server) getToken(w http.ResponseWriter, r *http.Request) {
	tokenRow, ok := s.authorizeToken(w, r, auth.PermissionTokensRead)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse(tokenRow))
}

func (s *Server) completeToken(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token completion JSON: %w", err)))
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
	completed, err := s.completeTokenRecord(
		r.Context(), tokenRow, request.Result, idempotencyKey, nil,
	)
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.CompleteTokenResponse{
		Status: "completed", Token: tokenResponse(completed),
	})
}

func (s *Server) cancelToken(w http.ResponseWriter, r *http.Request) {
	var request api.CancelTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token cancellation JSON: %w", err)))
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
	cancelled, err := s.cancelTokenRecord(r.Context(), tokenRow, idempotencyKey)
	if err != nil {
		s.writeTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse(cancelled))
}

func (s *Server) completeTokenWithCallback(w http.ResponseWriter, r *http.Request) {
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token callback JSON: %w", err)))
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
	completed, err := s.completeTokenRecord(r.Context(), db.Token{}, request.Result, "", func(
		ctx context.Context,
		q db.Querier,
	) (db.Token, *db.PublicAccessToken, error) {
		fingerprint := auth.HashCredential(callbackSecret)
		tokenRow, err := q.GetTokenForCallbackCompletion(ctx, db.GetTokenForCallbackCompletionParams{
			ID: pgvalue.UUID(tokenID), CallbackSecretFingerprint: fingerprint,
		})
		if err != nil {
			return db.Token{}, nil, errTokenScopeDenied
		}
		credentials, err := s.tokenCredentialKey.Derive(pgvalue.MustUUIDValue(tokenRow.ID))
		if err != nil ||
			!hmac.Equal(credentials.CallbackFingerprint, tokenRow.CallbackSecretFingerprint) ||
			!hmac.Equal(credentials.CallbackFingerprint, fingerprint) {
			return db.Token{}, nil, errTokenScopeDenied
		}
		return tokenRow, nil, nil
	})
	if err != nil {
		s.writePublicTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.CompleteTokenResponse{
		Status: "completed", Token: tokenResponse(completed),
	})
}

func (s *Server) completeTokenWithBearer(w http.ResponseWriter, r *http.Request) {
	s.writeTokenCORS(w)
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid Token completion JSON: %w", err)))
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
	completed, err := s.completeTokenRecord(r.Context(), db.Token{}, request.Result, "", func(
		ctx context.Context,
		q db.Querier,
	) (db.Token, *db.PublicAccessToken, error) {
		publicAccess, err := q.LockPublicAccessTokenByHash(
			ctx, auth.HashCredential(rawBearer),
		)
		if err != nil {
			return db.Token{}, nil, errTokenScopeDenied
		}
		tokenRow, err := q.GetTokenByID(ctx, pgvalue.UUID(tokenID))
		if err != nil {
			return db.Token{}, nil, errTokenScopeDenied
		}
		if publicAccess.TokenID != tokenRow.ID {
			return db.Token{}, nil, errTokenScopeDenied
		}
		credentials, err := s.tokenCredentialKey.Derive(pgvalue.MustUUIDValue(tokenRow.ID))
		if err != nil || !hmac.Equal(credentials.PublicAccessHash, publicAccess.TokenHash) ||
			!hmac.Equal(credentials.PublicAccessHash, auth.HashCredential(rawBearer)) {
			return db.Token{}, nil, errTokenScopeDenied
		}
		return tokenRow, &publicAccess, nil
	})
	if err != nil {
		s.writePublicTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.CompleteTokenResponse{
		Status: "completed", Token: tokenResponse(completed),
	})
}

func (s *Server) completeTokenBearerPreflight(w http.ResponseWriter, _ *http.Request) {
	s.writeTokenCORS(w)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

type tokenCompletionAuthorizer func(
	context.Context,
	db.Querier,
) (db.Token, *db.PublicAccessToken, error)

func (s *Server) completeTokenRecord(
	ctx context.Context,
	tokenRow db.Token,
	rawResult json.RawMessage,
	idempotencyKey string,
	authorize tokenCompletionAuthorizer,
) (db.Token, error) {
	canonical, err := canonicalJSON(rawResult)
	if err != nil {
		return db.Token{}, badRequest(errors.New("result must be unambiguous JSON"))
	}
	fingerprint := sha256.Sum256(canonical)
	var completed db.Token
	var terminalError error
	err = s.inTx(ctx, func(work *txWork) error {
		var publicAccess *db.PublicAccessToken
		if authorize != nil {
			var err error
			tokenRow, publicAccess, err = authorize(ctx, work.q)
			if err != nil {
				return err
			}
		}
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if idempotencyKey != "" {
			tokenID := pgvalue.MustUUIDValue(tokenRow.ID)
			claimRequest, err := idempotency.NewTokenCompleteRequest(
				pgvalue.MustUUIDValue(tokenRow.EnvironmentID), tokenID, idempotencyKey, canonical,
			)
			if err != nil {
				return err
			}
			claims, err = idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := tokenOperationReceiptFromJSON(acquired.Claim.Receipt)
				if err != nil || replayed.TokenID != pgvalue.UUIDString(tokenRow.ID) ||
					replayed.Outcome != "completed" {
					return errTokenOperationReceipt
				}
				completed, err = work.q.GetTokenByID(ctx, tokenRow.ID)
				return err
			}
			if acquired.Claim.State == "failed" {
				replayed, err := tokenOperationReceiptFromJSON(acquired.Claim.Receipt)
				if err != nil || replayed.TokenID != pgvalue.UUIDString(tokenRow.ID) ||
					replayed.Outcome != "expired" {
					return errTokenOperationReceipt
				}
				return gone(errTokenExpired)
			}
			if acquired.Claim.State != "pending" {
				return errTokenOperationReceipt
			}
			claim = &acquired.Claim
		}
		row, err := work.q.CompleteToken(ctx, db.CompleteTokenParams{
			CompletionFingerprint: fingerprint[:],
			OrgID:                 tokenRow.OrgID, ProjectID: tokenRow.ProjectID,
			EnvironmentID: tokenRow.EnvironmentID, ID: tokenRow.ID,
			Result:          canonical,
			OutboxMessageID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		})
		if err != nil {
			return err
		}
		switch {
		case row.CompletionConflict:
			return conflict(errTokenCompletionConflict)
		case row.CompletionExpired:
			completed = tokenFromCompleteRow(row)
			if claim != nil {
				receipt, err := json.Marshal(tokenOperationReceipt{
					TokenID: pgvalue.UUIDString(tokenRow.ID),
					Outcome: "expired",
				})
				if err != nil {
					return err
				}
				if _, err := claims.Fail(ctx, *claim, receipt); err != nil {
					return err
				}
			}
			terminalError = gone(errTokenExpired)
			return nil
		case row.CompletionCancelled:
			return conflict(errTokenCancelled)
		}
		completed = tokenFromCompleteRow(row)
		if publicAccess != nil && row.ReconciliationEnqueued {
			if _, err := work.q.MarkPublicAccessTokenUsed(ctx, publicAccess.ID); err != nil {
				return errTokenScopeDenied
			}
		}
		if claim != nil {
			receipt, err := json.Marshal(tokenOperationReceipt{
				TokenID: pgvalue.UUIDString(tokenRow.ID),
				Outcome: "completed",
			})
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && terminalError != nil {
		err = terminalError
	}
	return completed, err
}

func (s *Server) cancelTokenRecord(
	ctx context.Context,
	tokenRow db.Token,
	idempotencyKey string,
) (db.Token, error) {
	var cancelled db.Token
	var terminalError error
	err := s.inTx(ctx, func(work *txWork) error {
		var claim *db.IdempotencyClaim
		var claims *idempotency.Transaction
		if idempotencyKey != "" {
			claimRequest, err := idempotency.NewTokenCancelRequest(
				pgvalue.MustUUIDValue(tokenRow.EnvironmentID),
				pgvalue.MustUUIDValue(tokenRow.ID),
				idempotencyKey,
			)
			if err != nil {
				return err
			}
			claims, err = idempotency.TransactionForQueries(work.q)
			if err != nil {
				return err
			}
			acquired, err := claims.Acquire(ctx, claimRequest)
			if err != nil {
				return err
			}
			if acquired.Claim.State == "completed" {
				replayed, err := tokenOperationReceiptFromJSON(acquired.Claim.Receipt)
				if err != nil || replayed.TokenID != pgvalue.UUIDString(tokenRow.ID) ||
					replayed.Outcome != "cancelled" {
					return errTokenOperationReceipt
				}
				cancelled, err = work.q.GetTokenByID(ctx, tokenRow.ID)
				return err
			}
			if acquired.Claim.State == "failed" {
				replayed, err := tokenOperationReceiptFromJSON(acquired.Claim.Receipt)
				if err != nil || replayed.TokenID != pgvalue.UUIDString(tokenRow.ID) ||
					replayed.Outcome != "expired" {
					return errTokenOperationReceipt
				}
				return gone(errTokenExpired)
			}
			if acquired.Claim.State != "pending" {
				return errTokenOperationReceipt
			}
			claim = &acquired.Claim
		}
		row, err := work.q.CancelToken(ctx, db.CancelTokenParams{
			OrgID: tokenRow.OrgID, ProjectID: tokenRow.ProjectID,
			EnvironmentID: tokenRow.EnvironmentID, ID: tokenRow.ID,
			OutboxMessageID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		})
		if err != nil {
			return err
		}
		switch {
		case row.CancellationExpired:
			cancelled = tokenFromCancelRow(row)
			if claim != nil {
				receipt, err := json.Marshal(tokenOperationReceipt{
					TokenID: pgvalue.UUIDString(tokenRow.ID),
					Outcome: "expired",
				})
				if err != nil {
					return err
				}
				if _, err := claims.Fail(ctx, *claim, receipt); err != nil {
					return err
				}
			}
			terminalError = gone(errTokenExpired)
			return nil
		case row.CancellationCompleted:
			return conflict(errTokenCompleted)
		}
		cancelled = tokenFromCancelRow(row)
		if claim != nil {
			receipt, err := json.Marshal(tokenOperationReceipt{
				TokenID: pgvalue.UUIDString(tokenRow.ID),
				Outcome: "cancelled",
			})
			if err != nil {
				return err
			}
			if _, err := claims.Complete(ctx, *claim, receipt); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && terminalError != nil {
		err = terminalError
	}
	return cancelled, err
}

func tokenOperationReceiptFromJSON(raw []byte) (tokenOperationReceipt, error) {
	var receipt tokenOperationReceipt
	if err := decodeClosedJSON(raw, &receipt); err != nil ||
		ids.Validate(receipt.TokenID) != nil ||
		(receipt.Outcome != "completed" &&
			receipt.Outcome != "cancelled" &&
			receipt.Outcome != "expired") {
		return tokenOperationReceipt{}, errTokenOperationReceipt
	}
	return receipt, nil
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
	actor := actorFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, actor, "", "")
	if err != nil {
		writeError(w, badRequest(err))
		return db.Token{}, false
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.Token{}, false
	}
	tokenRow, err := s.db.GetToken(r.Context(), db.GetTokenParams{
		OrgID:         pgvalue.UUID(actor.OrgID),
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		ID:            pgvalue.UUID(tokenID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(errTokenNotFound))
		return db.Token{}, false
	}
	if err != nil {
		writeError(w, errors.New("load Token"))
		return db.Token{}, false
	}
	return tokenRow, true
}

func tokenResponse(row db.Token) api.TokenResponse {
	var expiresAt *time.Time
	if row.ExpiresAt.Valid {
		value := row.ExpiresAt.Time.UTC()
		expiresAt = &value
	}
	var completedAt *time.Time
	if row.CompletedAt.Valid {
		value := row.CompletedAt.Time.UTC()
		completedAt = &value
	}
	response := api.TokenResponse{
		ID: pgvalue.UUIDString(row.ID), Status: string(row.State), TimeoutAt: expiresAt,
		Tags: append([]string{}, row.Tags...), Metadata: json.RawMessage(row.Metadata),
		CompletedAt: completedAt, CreatedAt: row.CreatedAt.Time.UTC(),
		UpdatedAt: row.UpdatedAt.Time.UTC(),
	}
	if len(row.Result) > 0 {
		response.Result = json.RawMessage(row.Result)
	}
	return response
}

func (s *Server) tokenCreateResponse(
	row db.Token,
	credentials auth.Credentials,
) api.TokenResponse {
	creation := row
	creation.State = db.TokenStatePending
	creation.Result = nil
	creation.Error = nil
	creation.CompletionFingerprint = nil
	creation.CompletedAt = pgtype.Timestamptz{}
	creation.ExpiredAt = pgtype.Timestamptz{}
	creation.CancelledAt = pgtype.Timestamptz{}
	creation.UpdatedAt = creation.CreatedAt
	response := tokenResponse(creation)
	response.PublicAccessToken = credentials.PublicAccessToken
	response.CallbackURL = s.publicURL.ResolveReference(&url.URL{
		Path: "/api/token-callbacks/" + pgvalue.UUIDString(row.ID) + "/" + credentials.CallbackSecret,
	}).String()
	return response
}

func tokenFromCompleteRow(row db.CompleteTokenRow) db.Token {
	return db.Token{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID,
		EnvironmentID: row.EnvironmentID, State: row.State, ExpiresAt: row.ExpiresAt,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CompletionFingerprint:     row.CompletionFingerprint,
		Result:                    row.Result, Error: row.Error, Metadata: row.Metadata, Tags: row.Tags,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt,
		ExpiredAt: row.ExpiredAt, CancelledAt: row.CancelledAt,
	}
}

func tokenFromCancelRow(row db.CancelTokenRow) db.Token {
	return db.Token{
		ID: row.ID, OrgID: row.OrgID, ProjectID: row.ProjectID,
		EnvironmentID: row.EnvironmentID, State: row.State, ExpiresAt: row.ExpiresAt,
		CallbackSecretFingerprint: row.CallbackSecretFingerprint,
		CompletionFingerprint:     row.CompletionFingerprint,
		Result:                    row.Result, Error: row.Error, Metadata: row.Metadata, Tags: row.Tags,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, CompletedAt: row.CompletedAt,
		ExpiredAt: row.ExpiredAt, CancelledAt: row.CancelledAt,
	}
}

func (s *Server) writeTokenError(w http.ResponseWriter, err error) {
	var conflictError idempotency.ConflictError
	switch {
	case errors.As(err, &conflictError):
		writeError(w, conflict(codedError{code: "idempotency_conflict", message: "idempotency key conflicts with an earlier Token operation"}))
	case errors.Is(err, errTokenCreateAuthority):
		writeError(w, conflict(errTokenCreateAuthority))
	case errors.Is(err, pgx.ErrNoRows), errors.Is(err, errTokenNotFound):
		writeError(w, notFound(errTokenNotFound))
	default:
		writeError(w, err)
	}
}

func (s *Server) writePublicTokenError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errTokenCompletionConflict):
		writeError(w, conflict(errTokenCompletionConflict))
	case errors.Is(err, errTokenExpired):
		writeError(w, gone(errTokenExpired))
	case errors.Is(err, errTokenCancelled):
		writeError(w, conflict(errTokenCancelled))
	default:
		writeError(w, unauthorized(errTokenScopeDenied))
	}
}

func (s *Server) writeTokenCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Vary", "Origin")
}
