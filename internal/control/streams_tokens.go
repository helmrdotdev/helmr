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
	"strconv"
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

const (
	defaultStreamListLimit = int32(100)
	defaultTokenListLimit  = int32(100)
	defaultTokenTimeout    = 7 * 24 * time.Hour
	tokenCallbackKeyID     = "default"
	publicAccessTokenTTL   = 30 * 24 * time.Hour
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

func (s *Server) listSessionStreams(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authorizeSessionStreamRequest(w, r, auth.PermissionSessionStreamsRead)
	if !ok {
		return
	}
	rows, err := s.db.ListTaskSessionStreams(r.Context(), db.ListTaskSessionStreamsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
	})
	if err != nil {
		writeError(w, errors.New("list session streams"))
		return
	}
	streams := make([]api.StreamResponse, 0, len(rows))
	for _, row := range rows {
		streams = append(streams, streamResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListSessionStreamsResponse{Streams: streams})
}

func (s *Server) appendSessionInputStream(w http.ResponseWriter, r *http.Request) {
	s.appendSessionStreamRecord(w, r, db.StreamDirectionInput, auth.PermissionSessionInputSend, db.StreamRecordSourceTypeApiKey, pgtype.UUID{})
}

func (s *Server) appendSessionOutputStream(w http.ResponseWriter, r *http.Request) {
	s.appendSessionStreamRecord(w, r, db.StreamDirectionOutput, auth.PermissionSessionOutputAppend, db.StreamRecordSourceTypeApiKey, pgtype.UUID{})
}

func (s *Server) listSessionInputStreamRecords(w http.ResponseWriter, r *http.Request) {
	s.listSessionStreamRecords(w, r, db.StreamDirectionInput)
}

func (s *Server) listSessionOutputStreamRecords(w http.ResponseWriter, r *http.Request) {
	s.listSessionStreamRecords(w, r, db.StreamDirectionOutput)
}

func (s *Server) readSessionOutputStreamRecord(w http.ResponseWriter, r *http.Request) {
	session, stream, ok := s.authorizeSessionStreamResource(w, r, auth.PermissionSessionStreamsRead, db.StreamDirectionOutput)
	if !ok {
		return
	}
	response, err := s.readOutputStreamRecord(r.Context(), s.db, session, stream, streamCorrelationQuery(r), r)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) appendSessionInputStreamWithPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessCORS(w, r)
	var request api.AppendStreamRecordRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid stream record request JSON: %w", err)))
		return
	}
	publicAccessToken, ok := bearerToken(r.Header.Get("authorization"))
	if !ok {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("begin public stream input transaction"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	session, stream, consumedToken, err := s.authorizePublicAccessTokenStream(r.Context(), store, publicAccessToken, db.PublicAccessTokenScopeTypeSessioninputsend, db.StreamDirectionInput, pgtype.Text{String: strings.TrimSpace(request.CorrelationID), Valid: strings.TrimSpace(request.CorrelationID) != ""})
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	tokenID := pgvalue.MustUUIDValue(consumedToken.ID).String()
	record, err := s.appendStreamRecord(r.Context(), store, session, stream, db.StreamDirectionInput, db.StreamRecordSourceTypePublicAccessToken, tokenID, consumedToken.ID, request)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit public stream input transaction"))
		return
	}
	s.publishSessionInputStreamWakeup(r.Context(), session.OrgID, stream.ID, record.Sequence)
	s.requeueResolvedRunWaits(r.Context(), session.OrgID)
	writeJSON(w, http.StatusCreated, appendStreamRecordResponse(record))
}

func (s *Server) readSessionOutputStreamWithPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessCORS(w, r)
	publicAccessToken, ok := bearerToken(r.Header.Get("authorization"))
	if !ok {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	correlationID := streamCorrelationQuery(r)
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("begin public stream output transaction"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	session, stream, _, err := s.authorizePublicAccessTokenStream(r.Context(), store, publicAccessToken, db.PublicAccessTokenScopeTypeSessionoutputread, db.StreamDirectionOutput, correlationID)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	response, err := s.readOutputStreamRecord(r.Context(), store, session, stream, correlationID, r)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit public stream output transaction"))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) publicSessionInputStreamPreflight(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessPreflight(w, r, "POST")
}

func (s *Server) publicSessionOutputStreamReadPreflight(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessPreflight(w, r, "GET")
}

func (s *Server) readOutputStreamRecord(ctx context.Context, store db.Querier, session db.TaskSession, stream db.Stream, correlationID pgtype.Text, r *http.Request) (api.ReadStreamRecordResponse, error) {
	after, err := streamAfterSequence(r)
	if err != nil {
		return api.ReadStreamRecordResponse{}, badRequest(err)
	}
	records, err := store.ListStreamRecords(ctx, db.ListStreamRecordsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		StreamID:      stream.ID,
		Direction:     db.StreamDirectionOutput,
		AfterSequence: after,
		CorrelationID: correlationID,
		LimitCount:    1,
	})
	if err != nil {
		return api.ReadStreamRecordResponse{}, errors.New("read output stream")
	}
	if len(records) == 0 {
		return api.ReadStreamRecordResponse{}, nil
	}
	response := streamRecordResponse(records[0])
	return api.ReadStreamRecordResponse{Record: &response}, nil
}

func (s *Server) appendSessionStreamRecord(w http.ResponseWriter, r *http.Request, direction db.StreamDirection, permission auth.Permission, sourceType db.StreamRecordSourceType, publicAccessTokenID pgtype.UUID) {
	session, stream, ok := s.authorizeSessionStreamResource(w, r, permission, direction)
	if !ok {
		return
	}
	var request api.AppendStreamRecordRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid stream record request JSON: %w", err)))
		return
	}
	store := s.db
	var tx controlTransaction
	if direction == db.StreamDirectionInput {
		var err error
		store, tx, err = s.beginControlTransaction(r.Context())
		if err != nil {
			writeError(w, errors.New("begin stream input transaction"))
			return
		}
		defer func() { _ = tx.Rollback(r.Context()) }()
	}
	record, err := s.appendStreamRecord(r.Context(), store, session, stream, direction, sourceType, string(sourceType), publicAccessTokenID, request)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	if tx != nil {
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, errors.New("commit stream input transaction"))
			return
		}
	}
	if direction == db.StreamDirectionInput {
		s.publishSessionInputStreamWakeup(r.Context(), session.OrgID, stream.ID, record.Sequence)
		s.requeueResolvedRunWaits(r.Context(), session.OrgID)
	}
	writeJSON(w, http.StatusCreated, appendStreamRecordResponse(record))
}

func (s *Server) appendStreamRecord(ctx context.Context, store db.Querier, session db.TaskSession, stream db.Stream, direction db.StreamDirection, sourceType db.StreamRecordSourceType, sourceID string, publicAccessTokenID pgtype.UUID, request api.AppendStreamRecordRequest) (db.AppendStreamRecordRow, error) {
	data := request.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	if !json.Valid(data) {
		return db.AppendStreamRecordRow{}, badRequest(errors.New("data must be valid JSON"))
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return db.AppendStreamRecordRow{}, badRequest(err)
	}
	correlationID := strings.TrimSpace(request.CorrelationID)
	contentType := firstNonEmptyString(strings.TrimSpace(request.ContentType), "application/json")
	fingerprint, err := streamRecordFingerprint(data, correlationID, contentType)
	if err != nil {
		return db.AppendStreamRecordRow{}, err
	}
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		sourceID = string(sourceType)
	}
	row, err := store.AppendStreamRecord(ctx, db.AppendStreamRecordParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  session.OrgID,
		ProjectID:              session.ProjectID,
		EnvironmentID:          session.EnvironmentID,
		StreamID:               stream.ID,
		Direction:              direction,
		Data:                   data,
		CorrelationID:          correlationID,
		ContentType:            contentType,
		IdempotencyKey:         idempotencyKey,
		IdempotencyFingerprint: fingerprint,
		SourceType:             sourceType,
		SourceID:               sourceID,
		PublicAccessTokenID:    publicAccessTokenID,
	})
	if isNoRows(err) {
		return db.AppendStreamRecordRow{}, errStreamDirectionMismatch
	}
	if err != nil {
		return db.AppendStreamRecordRow{}, err
	}
	if row.IdempotencyFingerprintMismatch {
		return row, conflict(errIdempotencyFingerprint)
	}
	if direction == db.StreamDirectionInput {
		if _, err := store.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			StreamID:      stream.ID,
		}); err != nil {
			return db.AppendStreamRecordRow{}, err
		}
	}
	return row, nil
}

func (s *Server) listSessionStreamRecords(w http.ResponseWriter, r *http.Request, direction db.StreamDirection) {
	session, stream, ok := s.authorizeSessionStreamResource(w, r, auth.PermissionSessionStreamsRead, direction)
	if !ok {
		return
	}
	after, err := streamAfterSequence(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	limit, err := optionalLimitQuery(r, defaultStreamListLimit)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	records, err := s.db.ListStreamRecords(r.Context(), db.ListStreamRecordsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		StreamID:      stream.ID,
		Direction:     direction,
		AfterSequence: after,
		CorrelationID: streamCorrelationQuery(r),
		LimitCount:    limit,
	})
	if err != nil {
		writeError(w, errors.New("list stream records"))
		return
	}
	out := make([]api.StreamRecordResponse, 0, len(records))
	for _, row := range records {
		out = append(out, streamRecordResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListStreamRecordsResponse{Records: out})
}

func (s *Server) authorizeSessionStreamRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.TaskSession, bool) {
	sessionID, err := parseUUIDParam(r, "sessionID")
	if err != nil {
		writeError(w, badRequest(err))
		return db.TaskSession{}, false
	}
	actor := actorFromContext(r.Context())
	session, err := s.db.GetTaskSessionByOrgID(r.Context(), db.GetTaskSessionByOrgIDParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(sessionID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errStreamNotFound))
		return db.TaskSession{}, false
	}
	if err != nil {
		writeError(w, errors.New("load task session"))
		return db.TaskSession{}, false
	}
	if err := s.requireActorScopeForRecord(r, actor, session.ProjectID, session.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errStreamNotFound))
			return db.TaskSession{}, false
		}
		writeError(w, badRequest(err))
		return db.TaskSession{}, false
	}
	scope := auth.Scope{OrgID: actor.OrgID, ProjectID: pgvalue.MustUUIDValue(session.ProjectID).String(), EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String()}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errPermissionRequired))
		return db.TaskSession{}, false
	}
	return session, true
}

func (s *Server) authorizeSessionStreamResource(w http.ResponseWriter, r *http.Request, permission auth.Permission, direction db.StreamDirection) (db.TaskSession, db.Stream, bool) {
	session, ok := s.authorizeSessionStreamRequest(w, r, permission)
	if !ok {
		return db.TaskSession{}, db.Stream{}, false
	}
	streamName := strings.TrimSpace(chi.URLParam(r, "stream"))
	if err := validateSessionStreamName(streamName); err != nil {
		writeError(w, badRequest(err))
		return db.TaskSession{}, db.Stream{}, false
	}
	stream, err := s.db.GetTaskSessionStreamByName(r.Context(), db.GetTaskSessionStreamByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          streamName,
		Direction:     direction,
	})
	if isNoRows(err) {
		opposite := db.StreamDirectionOutput
		if direction == db.StreamDirectionOutput {
			opposite = db.StreamDirectionInput
		}
		if _, oppositeErr := s.db.GetTaskSessionStreamByName(r.Context(), db.GetTaskSessionStreamByNameParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			TaskSessionID: session.ID,
			Name:          streamName,
			Direction:     opposite,
		}); oppositeErr == nil {
			writeError(w, conflict(errStreamDirectionMismatch))
			return db.TaskSession{}, db.Stream{}, false
		}
		writeError(w, notFound(errStreamNotFound))
		return db.TaskSession{}, db.Stream{}, false
	}
	if err != nil {
		writeError(w, errors.New("load stream"))
		return db.TaskSession{}, db.Stream{}, false
	}
	return session, stream, true
}

func (s *Server) createPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	var request api.CreatePublicAccessTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid public access token create request JSON: %w", err)))
		return
	}
	actor := actorFromContext(r.Context())
	session, stream, scopeType, correlationID, err := s.resolvePublicAccessTokenScopeRequest(r.Context(), r, actor, request.Scope)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	expiresAt := time.Now().Add(publicAccessTokenTTL)
	if request.ExpiresAt != nil {
		expiresAt = request.ExpiresAt.UTC()
	}
	if !expiresAt.After(time.Now()) {
		writeError(w, gone(errTokenExpired))
		return
	}
	maxUses := pgtype.Int4{}
	var responseMaxUses *int32
	if request.MaxUses != nil {
		if *request.MaxUses <= 0 {
			writeError(w, badRequest(errors.New("max_uses must be a positive integer")))
			return
		}
		maxUses = pgtype.Int4{Int32: *request.MaxUses, Valid: true}
		value := *request.MaxUses
		responseMaxUses = &value
	}
	rawToken, err := auth.GenerateOpaqueToken(32)
	if err != nil {
		writeError(w, errors.New("generate public access token"))
		return
	}
	rawToken = "hlmr_pat_" + rawToken
	tokenHash, err := auth.HashToken(s.authSecret, rawToken)
	if err != nil {
		writeError(w, errors.New("hash public access token"))
		return
	}
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("begin public access token create transaction"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	publicToken, err := store.CreatePublicAccessToken(r.Context(), db.CreatePublicAccessTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TokenHash:     tokenHash,
		ExpiresAt:     pgvalue.Timestamptz(expiresAt),
		MaxUses:       maxUses,
		Metadata:      []byte(`{}`),
		CreatedBy:     actorJSON(actor),
	})
	if err != nil {
		writeError(w, errors.New("create public access token"))
		return
	}
	scope, err := store.CreatePublicAccessTokenScope(r.Context(), db.CreatePublicAccessTokenScopeParams{
		ID:                  pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               session.OrgID,
		ProjectID:           session.ProjectID,
		EnvironmentID:       session.EnvironmentID,
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           scopeType,
		StreamID:            stream.ID,
		CorrelationID:       correlationID,
	})
	if isNoRows(err) {
		writeError(w, forbidden(errTokenScopeDenied))
		return
	}
	if err != nil {
		writeError(w, errors.New("create public access token scope"))
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit public access token create transaction"))
		return
	}
	writeJSON(w, http.StatusCreated, api.PublicAccessTokenResponse{
		ID:                pgvalue.MustUUIDValue(publicToken.ID).String(),
		PublicAccessToken: rawToken,
		Scope: api.PublicAccessTokenScopeResponse{
			Type:          string(scope.ScopeType),
			SessionID:     pgvalue.MustUUIDValue(session.ID).String(),
			Stream:        stream.Name,
			CorrelationID: scope.CorrelationID,
		},
		ExpiresAt: pgvalue.Time(publicToken.ExpiresAt),
		MaxUses:   responseMaxUses,
		CreatedAt: pgvalue.Time(publicToken.CreatedAt),
	})
}

func (s *Server) resolvePublicAccessTokenScopeRequest(ctx context.Context, r *http.Request, actor auth.Actor, request api.PublicAccessTokenScopeRequest) (db.TaskSession, db.Stream, db.PublicAccessTokenScopeType, pgtype.Text, error) {
	sessionID, err := uuid.Parse(strings.TrimSpace(request.SessionID))
	if err != nil {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, badRequest(errors.New("scope.session_id must be a UUID"))
	}
	direction, permission, scopeType, err := publicAccessTokenScopeContract(request.Type)
	if err != nil {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	streamName := strings.TrimSpace(request.Stream)
	if err := validateSessionStreamName(streamName); err != nil {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	session, err := s.db.GetTaskSessionByOrgID(ctx, db.GetTaskSessionByOrgIDParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(sessionID),
	})
	if isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, err
	}
	if err := s.requireActorScopeForRecord(r, actor, session.ProjectID, session.EnvironmentID); err != nil {
		if isNoRows(err) {
			return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, notFound(errStreamNotFound)
		}
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	scope := auth.Scope{OrgID: actor.OrgID, ProjectID: pgvalue.MustUUIDValue(session.ProjectID).String(), EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String()}
	if !actor.HasPermission(permission, scope) {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, forbidden(errPermissionRequired)
	}
	stream, err := s.db.GetTaskSessionStreamByName(ctx, db.GetTaskSessionStreamByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          streamName,
		Direction:     direction,
	})
	if isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.TaskSession{}, db.Stream{}, "", pgtype.Text{}, err
	}
	correlationID := strings.TrimSpace(request.CorrelationID)
	return session, stream, scopeType, pgtype.Text{String: correlationID, Valid: correlationID != ""}, nil
}

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
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("begin token create transaction"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	token, publicToken, err := s.createTokenRecord(r.Context(), store, actor, projectID, environmentID, request)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit token create transaction"))
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
	writeJSON(w, http.StatusOK, tokenResponse(tokenFromCompleteRow(completed), "", ""))
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
	s.requeueResolvedRunWaits(r.Context(), token.OrgID)
	writeJSON(w, http.StatusOK, tokenResponse(tokenFromCancelRow(cancelled), "", ""))
}

func (s *Server) completeTokenWithPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserCompletionCORS(w, r)
	var request api.CompleteTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid token complete request JSON: %w", err)))
		return
	}
	tokenID, err := parseUUIDParam(r, "tokenID")
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	token, err := s.db.GetTokenByID(r.Context(), pgvalue.UUID(tokenID))
	if isNoRows(err) {
		writeError(w, notFound(errTokenNotFound))
		return
	}
	if err != nil {
		writeError(w, errors.New("load token"))
		return
	}
	publicAccessToken, ok := bearerToken(r.Header.Get("authorization"))
	if !ok {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	store, tx, err := s.beginControlTransaction(r.Context())
	if err != nil {
		writeError(w, errors.New("begin public token completion transaction"))
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	_, err = s.consumePublicAccessTokenScope(r.Context(), store, publicAccessToken, token, db.PublicAccessTokenScopeTypeTokencomplete)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	completed, err := s.completeTokenRecord(r.Context(), store, token, request.Data)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeError(w, errors.New("commit public token completion transaction"))
		return
	}
	s.requeueResolvedRunWaits(r.Context(), token.OrgID)
	writeJSON(w, http.StatusOK, tokenResponse(tokenFromCompleteRow(completed), "", ""))
}

func (s *Server) completeTokenPublicAccessTokenPreflight(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessPreflight(w, r, "POST")
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
	writeJSON(w, http.StatusOK, tokenResponse(tokenFromCompleteRow(completed), "", ""))
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
	return completed, nil
}

func (s *Server) consumePublicAccessTokenScope(ctx context.Context, store db.Querier, raw string, token db.Token, scopeType db.PublicAccessTokenScopeType) (db.PublicAccessToken, error) {
	if scopeType != db.PublicAccessTokenScopeTypeTokencomplete {
		return db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	}
	publicToken, err := s.lockActivePublicAccessToken(ctx, store, raw)
	if err != nil {
		return db.PublicAccessToken{}, err
	}
	if _, err := store.GetPublicAccessTokenTokenScope(ctx, db.GetPublicAccessTokenTokenScopeParams{
		OrgID:               token.OrgID,
		ProjectID:           token.ProjectID,
		EnvironmentID:       token.EnvironmentID,
		PublicAccessTokenID: publicToken.ID,
		TokenID:             token.ID,
	}); isNoRows(err) {
		return db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	} else if err != nil {
		return db.PublicAccessToken{}, err
	}
	consumed, err := store.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	})
	if isNoRows(err) {
		return db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	}
	if err != nil {
		return db.PublicAccessToken{}, err
	}
	return consumed, nil
}

func (s *Server) authorizePublicAccessTokenStream(ctx context.Context, store db.Querier, raw string, scopeType db.PublicAccessTokenScopeType, direction db.StreamDirection, correlationID pgtype.Text) (db.TaskSession, db.Stream, db.PublicAccessToken, error) {
	sessionID, err := parseUUIDString(strings.TrimSpace(chi.URLParamFromCtx(ctx, "sessionID")), "sessionID")
	if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, badRequest(err)
	}
	streamName := strings.TrimSpace(chi.URLParamFromCtx(ctx, "stream"))
	if err := validateSessionStreamName(streamName); err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, badRequest(err)
	}
	publicToken, err := s.lockActivePublicAccessToken(ctx, store, raw)
	if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	session, err := store.GetTaskSessionByOrgID(ctx, db.GetTaskSessionByOrgIDParams{
		OrgID: publicToken.OrgID,
		ID:    pgvalue.UUID(sessionID),
	})
	if isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	stream, err := store.GetTaskSessionStreamByName(ctx, db.GetTaskSessionStreamByNameParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          streamName,
		Direction:     direction,
	})
	if isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	if _, err := store.GetPublicAccessTokenStreamScope(ctx, db.GetPublicAccessTokenStreamScopeParams{
		OrgID:               publicToken.OrgID,
		ProjectID:           publicToken.ProjectID,
		EnvironmentID:       publicToken.EnvironmentID,
		PublicAccessTokenID: publicToken.ID,
		ScopeType:           scopeType,
		StreamID:            stream.ID,
		CorrelationID:       correlationID,
	}); isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	} else if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	consumed, err := store.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	})
	if isNoRows(err) {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	}
	if err != nil {
		return db.TaskSession{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	return session, stream, consumed, nil
}

func (s *Server) lockActivePublicAccessToken(ctx context.Context, store db.Querier, raw string) (db.PublicAccessToken, error) {
	tokenHash, err := auth.HashToken(s.authSecret, raw)
	if err != nil {
		return db.PublicAccessToken{}, unauthorized(errTokenScopeDenied)
	}
	publicToken, err := store.LockPublicAccessTokenByHash(ctx, tokenHash)
	if isNoRows(err) {
		return db.PublicAccessToken{}, unauthorized(errTokenScopeDenied)
	}
	if err != nil {
		return db.PublicAccessToken{}, err
	}
	if publicToken.State != db.PublicAccessTokenStateActive {
		return db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	}
	if !pgvalue.Time(publicToken.ExpiresAt).After(time.Now()) {
		return db.PublicAccessToken{}, gone(errTokenExpired)
	}
	return publicToken, nil
}

func streamResponse(row db.Stream) api.StreamResponse {
	return api.StreamResponse{
		ID:        pgvalue.MustUUIDValue(row.ID).String(),
		SessionID: pgvalue.MustUUIDValue(row.SessionID).String(),
		Name:      row.Name,
		Direction: string(row.Direction),
		Sequence:  row.NextSequence - 1,
		Metadata:  json.RawMessage(row.Metadata),
		CreatedAt: pgvalue.Time(row.CreatedAt),
	}
}

func appendStreamRecordResponse(row db.AppendStreamRecordRow) api.AppendStreamRecordResponse {
	status := "created"
	if row.IsCached {
		status = "duplicate"
	}
	return api.AppendStreamRecordResponse{
		Record:            streamRecordResponseFields(row.ID, row.StreamID, row.Sequence, row.Data, row.CorrelationID, row.ContentType, row.CreatedAt),
		IdempotencyStatus: status,
	}
}

func streamRecordResponse(row db.StreamRecord) api.StreamRecordResponse {
	return streamRecordResponseFields(row.ID, row.StreamID, row.Sequence, row.Data, row.CorrelationID, row.ContentType, row.CreatedAt)
}

func streamRecordResponseFields(id pgtype.UUID, streamID pgtype.UUID, sequence int64, data []byte, correlationID string, contentType string, createdAt pgtype.Timestamptz) api.StreamRecordResponse {
	return api.StreamRecordResponse{
		ID:            pgvalue.MustUUIDValue(id).String(),
		StreamID:      pgvalue.MustUUIDValue(streamID).String(),
		Sequence:      sequence,
		Data:          json.RawMessage(data),
		CorrelationID: correlationID,
		ContentType:   contentType,
		CreatedAt:     pgvalue.Time(createdAt),
	}
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

func streamRecordFingerprint(data json.RawMessage, correlationID string, contentType string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"content_type":   contentType,
		"correlation_id": correlationID,
		"data":           json.RawMessage(data),
	})
	if err != nil {
		return "", err
	}
	return jsonFingerprint(payload)
}

func jsonFingerprint(raw []byte) (string, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func streamAfterSequence(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(firstNonEmptyString(r.URL.Query().Get("after_sequence"), r.URL.Query().Get("cursor")))
	if raw == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errors.New("after_sequence must be a non-negative integer")
	}
	return parsed, nil
}

func streamCorrelationQuery(r *http.Request) pgtype.Text {
	if raw := strings.TrimSpace(r.URL.Query().Get("correlation_id")); raw != "" {
		return pgtype.Text{String: raw, Valid: true}
	}
	return pgtype.Text{}
}

func parseUUIDString(raw string, label string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a UUID", label)
	}
	return id, nil
}

func publicAccessTokenScopeContract(raw string) (db.StreamDirection, auth.Permission, db.PublicAccessTokenScopeType, error) {
	switch db.PublicAccessTokenScopeType(strings.TrimSpace(raw)) {
	case db.PublicAccessTokenScopeTypeSessioninputsend:
		return db.StreamDirectionInput, auth.PermissionSessionInputSend, db.PublicAccessTokenScopeTypeSessioninputsend, nil
	case db.PublicAccessTokenScopeTypeSessionoutputread:
		return db.StreamDirectionOutput, auth.PermissionSessionStreamsRead, db.PublicAccessTokenScopeTypeSessionoutputread, nil
	default:
		return "", "", "", fmt.Errorf("unsupported public access token scope type %q", raw)
	}
}

func actorJSON(actor auth.Actor) []byte {
	actorID := ""
	switch actor.Kind {
	case auth.ActorKindAPIKey:
		actorID = actor.APIKeyID.String()
	case auth.ActorKindSession:
		actorID = actor.SessionID.String()
	default:
		actorID = actor.UserID.String()
	}
	payload, err := json.Marshal(map[string]string{
		"kind": string(actor.Kind),
		"id":   actorID,
	})
	if err != nil {
		return []byte(`{}`)
	}
	return payload
}

func normalizeIdempotencyKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if len(key) > maxIdempotencyKeyLength {
		return "", fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return key, nil
}

func validateSessionStreamName(name string) error {
	return api.ValidateStreamName(name)
}

func (s *Server) writeBrowserCompletionCORS(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessCORS(w, r)
}

func (s *Server) writeBrowserPublicAccessCORS(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("origin"))
	if origin == "" || s.publicURL == nil {
		return
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return
	}
	if strings.EqualFold(parsed.Scheme, s.publicURL.Scheme) && strings.EqualFold(parsed.Host, s.publicURL.Host) {
		w.Header().Set("access-control-allow-origin", origin)
		w.Header().Set("vary", "origin")
	}
}

func (s *Server) writeBrowserPublicAccessPreflight(w http.ResponseWriter, r *http.Request, method string) {
	s.writeBrowserPublicAccessCORS(w, r)
	if w.Header().Get("access-control-allow-origin") == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("access-control-allow-methods", method+", OPTIONS")
	w.Header().Set("access-control-allow-headers", "authorization, content-type, "+api.APIVersionHeader)
	w.Header().Set("access-control-max-age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeStreamTokenError(w http.ResponseWriter, err error) {
	if errors.Is(err, errTokenNotFound) || errors.Is(err, errStreamNotFound) {
		writeError(w, notFound(err))
		return
	}
	writeError(w, err)
}
