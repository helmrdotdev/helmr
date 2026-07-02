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
	"github.com/helmrdotdev/helmr/internal/token"
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
	if err := s.materializeSessionStreamCatalog(r.Context(), session); err != nil {
		writeError(w, errors.New("list session streams"))
		return
	}
	rows, err := s.db.ListSessionStreams(r.Context(), db.ListSessionStreamsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		SessionID:     session.ID,
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

func (s *Server) materializeSessionStreamCatalog(ctx context.Context, session db.Session) error {
	if !session.ActiveDeploymentID.Valid {
		return nil
	}
	deploymentStreams, err := s.db.ListDeploymentStreamsForDeployment(ctx, db.ListDeploymentStreamsForDeploymentParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		DeploymentID:  session.ActiveDeploymentID,
	})
	if err != nil {
		return err
	}
	for _, deploymentStream := range deploymentStreams {
		if _, err := s.db.EnsureSessionStream(ctx, db.EnsureSessionStreamParams{
			ID:                 pgvalue.UUID(uuid.New()),
			OrgID:              session.OrgID,
			ProjectID:          session.ProjectID,
			EnvironmentID:      session.EnvironmentID,
			SessionID:          session.ID,
			DeploymentStreamID: deploymentStream.ID,
			Metadata:           []byte("{}"),
		}); err != nil {
			return err
		}
	}
	return nil
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
	var appended appendedStreamRecord
	err := s.inTx(r.Context(), func(work *txWork) error {
		session, stream, consumedToken, err := s.authorizePublicAccessTokenStream(r.Context(), r, work.q, publicAccessToken, db.PublicAccessTokenScopeTypeSessioninputsend, db.StreamDirectionInput, pgtype.Text{String: strings.TrimSpace(request.CorrelationID), Valid: strings.TrimSpace(request.CorrelationID) != ""})
		if err != nil {
			return err
		}
		tokenID := pgvalue.MustUUIDValue(consumedToken.ID).String()
		appended, err = s.appendStreamRecord(r.Context(), work.q, session, stream, db.StreamDirectionInput, db.StreamRecordSourceTypePublicAccessToken, tokenID, consumedToken.ID, request)
		if err != nil {
			return err
		}
		work.AfterCommit(func(ctx context.Context) error {
			s.publishSessionInputStreamWakeup(ctx, session.OrgID, stream.ID, appended.record.Sequence)
			if appended.resolvedWaitCount > 0 {
				s.requeueResolvedRunWaits(ctx, session.OrgID)
			}
			for _, runID := range s.sessionRunRequestWorkflow().reconcileAccepted(ctx, session.OrgID, session.ProjectID, session.EnvironmentID, session.ID) {
				appended.continuationRunID = runID
				appended.continuationStatus = "created"
			}
			return nil
		})
		return nil
	})
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, appendStreamRecordResponse(appended.record, appended.continuationStatus))
}

func (s *Server) readSessionOutputStreamWithPublicAccessToken(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessCORS(w, r)
	publicAccessToken, ok := bearerToken(r.Header.Get("authorization"))
	if !ok {
		writeError(w, unauthorized(errTokenScopeDenied))
		return
	}
	correlationID := streamCorrelationQuery(r)
	var response api.ReadStreamRecordResponse
	err := s.inTx(r.Context(), func(work *txWork) error {
		session, stream, _, err := s.authorizePublicAccessTokenStream(r.Context(), r, work.q, publicAccessToken, db.PublicAccessTokenScopeTypeSessionoutputread, db.StreamDirectionOutput, correlationID)
		if err != nil {
			return err
		}
		response, err = s.readOutputStreamRecord(r.Context(), work.q, session, stream, correlationID, r)
		return err
	})
	if err != nil {
		s.writeStreamTokenError(w, err)
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

func (s *Server) readOutputStreamRecord(ctx context.Context, store db.Querier, session db.Session, stream db.Stream, correlationID pgtype.Text, r *http.Request) (api.ReadStreamRecordResponse, error) {
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
	if direction == db.StreamDirectionInput {
		var appended appendedStreamRecord
		err := s.inTx(r.Context(), func(work *txWork) error {
			var err error
			appended, err = s.appendStreamRecord(r.Context(), work.q, session, stream, direction, sourceType, string(sourceType), publicAccessTokenID, request)
			if err != nil {
				return err
			}
			work.AfterCommit(func(ctx context.Context) error {
				s.publishSessionInputStreamWakeup(ctx, session.OrgID, stream.ID, appended.record.Sequence)
				if appended.resolvedWaitCount > 0 {
					s.requeueResolvedRunWaits(ctx, session.OrgID)
				}
				for _, runID := range s.sessionRunRequestWorkflow().reconcileAccepted(ctx, session.OrgID, session.ProjectID, session.EnvironmentID, session.ID) {
					appended.continuationRunID = runID
					appended.continuationStatus = "created"
				}
				return nil
			})
			return nil
		})
		if err != nil {
			s.writeStreamTokenError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, appendStreamRecordResponse(appended.record, appended.continuationStatus))
		return
	}
	appended, err := s.appendStreamRecord(r.Context(), s.db, session, stream, direction, sourceType, string(sourceType), publicAccessTokenID, request)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, appendStreamRecordResponse(appended.record, appended.continuationStatus))
}

type appendedStreamRecord struct {
	record             db.AppendStreamRecordRow
	resolvedWaitCount  int
	continuationRunID  pgtype.UUID
	continuationStatus string
}

func (s *Server) appendStreamRecord(ctx context.Context, store db.Querier, session db.Session, stream db.Stream, direction db.StreamDirection, sourceType db.StreamRecordSourceType, sourceID string, publicAccessTokenID pgtype.UUID, request api.AppendStreamRecordRequest) (appendedStreamRecord, error) {
	if direction == db.StreamDirectionInput {
		locked, err := store.LockSession(ctx, db.LockSessionParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		})
		if err != nil {
			return appendedStreamRecord{}, err
		}
		if locked.Status == db.SessionStatusExpired {
			return appendedStreamRecord{}, gone(errSessionExpired)
		}
		if locked.Status != db.SessionStatusOpen {
			return appendedStreamRecord{}, conflict(errSessionTerminated)
		}
		if locked.ExpiresAt.Valid && !locked.ExpiresAt.Time.After(time.Now()) {
			return appendedStreamRecord{}, gone(errSessionExpired)
		}
		session = locked
	}
	data := request.Data
	if len(data) == 0 {
		data = json.RawMessage(`null`)
	}
	if !json.Valid(data) {
		return appendedStreamRecord{}, badRequest(errors.New("data must be valid JSON"))
	}
	idempotencyKey, err := normalizeIdempotencyKey(request.IdempotencyKey)
	if err != nil {
		return appendedStreamRecord{}, badRequest(err)
	}
	correlationID := strings.TrimSpace(request.CorrelationID)
	contentType := firstNonEmptyString(strings.TrimSpace(request.ContentType), "application/json")
	fingerprint, err := streamRecordFingerprint(data, correlationID, contentType)
	if err != nil {
		return appendedStreamRecord{}, err
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
		return appendedStreamRecord{}, errStreamDirectionMismatch
	}
	if err != nil {
		return appendedStreamRecord{}, err
	}
	if row.IdempotencyFingerprintMismatch {
		return appendedStreamRecord{record: row}, conflict(errIdempotencyFingerprint)
	}
	appended := appendedStreamRecord{record: row}
	if direction == db.StreamDirectionInput {
		resolved, err := store.ResolveStreamWaitsForStream(ctx, db.ResolveStreamWaitsForStreamParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			StreamID:      stream.ID,
		})
		if err != nil {
			return appendedStreamRecord{}, err
		}
		appended.resolvedWaitCount = len(resolved)
		if appended.resolvedWaitCount > 0 {
			if _, err := store.CreateResolvedLiveRuntimeResumeWaitCommandsForOrg(ctx, db.CreateResolvedLiveRuntimeResumeWaitCommandsForOrgParams{
				OrgID:      session.OrgID,
				LimitCount: int32(appended.resolvedWaitCount),
			}); err != nil {
				return appendedStreamRecord{}, err
			}
		}
		if !row.IsCached && appended.resolvedWaitCount == 0 {
			if _, err := store.EnsureSessionRunRequestForStreamRecord(ctx, db.EnsureSessionRunRequestForStreamRecordParams{
				ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:          session.OrgID,
				ProjectID:      session.ProjectID,
				EnvironmentID:  session.EnvironmentID,
				SessionID:      session.ID,
				StreamRecordID: row.ID,
				StreamID:       stream.ID,
			}); err != nil {
				return appendedStreamRecord{}, err
			}
			appended.continuationStatus = "accepted_run_pending"
		} else if row.IsCached {
			appended.continuationStatus = "duplicate"
		}
	}
	return appended, nil
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

func (s *Server) authorizeSessionStreamRequest(w http.ResponseWriter, r *http.Request, permission auth.Permission) (db.Session, bool) {
	return s.loadSessionForRequest(w, r, permission)
}

func (s *Server) authorizeSessionStreamResource(w http.ResponseWriter, r *http.Request, permission auth.Permission, direction db.StreamDirection) (db.Session, db.Stream, bool) {
	session, ok := s.authorizeSessionStreamRequest(w, r, permission)
	if !ok {
		return db.Session{}, db.Stream{}, false
	}
	streamName := strings.TrimSpace(chi.URLParam(r, "stream"))
	if err := validateSessionStreamName(streamName); err != nil {
		writeError(w, badRequest(err))
		return db.Session{}, db.Stream{}, false
	}
	stream, err := s.ensureSessionStream(r.Context(), s.db, session, session.ActiveDeploymentID, streamName, direction)
	if err != nil {
		s.writeStreamTokenError(w, err)
		return db.Session{}, db.Stream{}, false
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
	session, stream, scopeType, correlationID, err := s.resolvePublicAccessTokenScopeRequest(r.Context(), actor, request)
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
	rawToken, err := token.GenerateOpaque(32)
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
	var publicToken db.PublicAccessToken
	var scope db.PublicAccessTokenScope
	err = s.inTx(r.Context(), func(work *txWork) error {
		var err error
		publicToken, err = work.q.CreatePublicAccessToken(r.Context(), db.CreatePublicAccessTokenParams{
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
			return errors.New("create public access token")
		}
		scope, err = work.q.CreatePublicAccessTokenScope(r.Context(), db.CreatePublicAccessTokenScopeParams{
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
			return forbidden(errTokenScopeDenied)
		}
		if err != nil {
			return errors.New("create public access token scope")
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.PublicAccessTokenResponse{
		ID:                pgvalue.MustUUIDValue(publicToken.ID).String(),
		PublicAccessToken: rawToken,
		Scope: api.PublicAccessTokenScopeResponse{
			Type:          string(scope.ScopeType),
			Session:       sessionAddressResponse(session),
			Stream:        stream.Name,
			CorrelationID: scope.CorrelationID,
		},
		ExpiresAt: pgvalue.Time(publicToken.ExpiresAt),
		MaxUses:   responseMaxUses,
		CreatedAt: pgvalue.Time(publicToken.CreatedAt),
	})
}

func (s *Server) resolvePublicAccessTokenScopeRequest(ctx context.Context, actor auth.Actor, request api.CreatePublicAccessTokenRequest) (db.Session, db.Stream, db.PublicAccessTokenScopeType, pgtype.Text, error) {
	scopeRequest := request.Scope
	address, err := sessionAddressFromAPIAddress(scopeRequest.Session)
	if err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	direction, permission, scopeType, err := publicAccessTokenScopeContract(scopeRequest.Type)
	if err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	streamName := strings.TrimSpace(scopeRequest.Stream)
	if err := validateSessionStreamName(streamName); err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	var session db.Session
	requestProjectID := strings.TrimSpace(request.ProjectID)
	requestEnvironmentID := strings.TrimSpace(request.EnvironmentID)
	hasRequestedScope := requestProjectID != "" || requestEnvironmentID != ""
	if actor.Kind != auth.ActorKindAPIKey && (requestProjectID == "" || requestEnvironmentID == "") {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(errors.New("project_id and environment_id are required for public token session resolution"))
	}
	if hasRequestedScope {
		_, projectID, environmentID, scopeErr := s.requestEnvironmentScope(ctx, actor, request.ProjectID, request.EnvironmentID)
		if scopeErr != nil {
			return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(scopeErr)
		}
		session, err = s.loadSessionAddressInScope(ctx, actor.OrgID, projectID, environmentID, address)
	} else {
		session, err = s.loadSessionByActorAddress(ctx, actor, address)
	}
	if isNoRows(err) {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, notFound(errStreamNotFound)
	}
	if err != nil && (errors.Is(err, errSessionExternalIDScopeRequired) || errors.Is(err, errAPIKeyEnvironmentScopeRequired)) {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, badRequest(err)
	}
	if err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, err
	}
	scope := auth.Scope{OrgID: actor.OrgID, ProjectID: pgvalue.MustUUIDValue(session.ProjectID).String(), EnvironmentID: pgvalue.MustUUIDValue(session.EnvironmentID).String()}
	if !actor.HasPermission(permission, scope) {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, forbidden(errPermissionRequired)
	}
	stream, err := s.ensureSessionStream(ctx, s.db, session, session.ActiveDeploymentID, streamName, direction)
	if err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, err
	}
	correlationID := strings.TrimSpace(scopeRequest.CorrelationID)
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
	var completed db.CompleteTokenRow
	err = s.inTx(r.Context(), func(work *txWork) error {
		publicToken, err := s.authorizePublicAccessTokenScope(r.Context(), work.q, publicAccessToken, token, db.PublicAccessTokenScopeTypeTokencomplete)
		if err != nil {
			return err
		}
		completed, err = s.completeTokenRecord(r.Context(), work.q, token, request.Data)
		if err != nil {
			return err
		}
		if !completed.AlreadyCompleted {
			if _, err := s.consumePublicAccessToken(r.Context(), work.q, publicToken); err != nil {
				return err
			}
		}
		work.AfterCommit(func(ctx context.Context) error {
			s.requeueResolvedRunWaits(ctx, token.OrgID)
			return nil
		})
		return nil
	})
	if err != nil {
		s.writeStreamTokenError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, tokenCompleteResponse(completed))
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

func (s *Server) authorizePublicAccessTokenScope(ctx context.Context, store db.Querier, raw string, token db.Token, scopeType db.PublicAccessTokenScopeType) (db.PublicAccessToken, error) {
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
	return publicToken, nil
}

func (s *Server) consumePublicAccessToken(ctx context.Context, store db.Querier, publicToken db.PublicAccessToken) (db.PublicAccessToken, error) {
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

func (s *Server) authorizePublicAccessTokenStream(ctx context.Context, r *http.Request, store db.Querier, raw string, scopeType db.PublicAccessTokenScopeType, direction db.StreamDirection, correlationID pgtype.Text) (db.Session, db.Stream, db.PublicAccessToken, error) {
	address, err := sessionAddressFromRequest(r)
	if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, badRequest(err)
	}
	streamName := strings.TrimSpace(chi.URLParamFromCtx(ctx, "stream"))
	if err := validateSessionStreamName(streamName); err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, badRequest(err)
	}
	publicToken, err := s.lockActivePublicAccessToken(ctx, store, raw)
	if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	session, err := loadSessionAddressInScope(ctx, store, pgvalue.MustUUIDValue(publicToken.OrgID), publicToken.ProjectID, publicToken.EnvironmentID, address)
	if isNoRows(err) {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	stream, err := store.GetSessionStreamByName(ctx, db.GetSessionStreamByNameParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		SessionID:     session.ID,
		Name:          streamName,
		Direction:     direction,
	})
	if isNoRows(err) {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, notFound(errStreamNotFound)
	}
	if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
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
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	} else if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	consumed, err := store.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	})
	if isNoRows(err) {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, forbidden(errTokenScopeDenied)
	}
	if err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
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

func appendStreamRecordResponse(row db.AppendStreamRecordRow, continuationStatus string) api.AppendStreamRecordResponse {
	status := "created"
	if row.IsCached {
		status = "duplicate"
	}
	return api.AppendStreamRecordResponse{
		Record:             streamRecordResponseFields(row.ID, row.StreamID, row.Sequence, row.Data, row.CorrelationID, row.ContentType, row.CreatedAt),
		IdempotencyStatus:  status,
		ContinuationStatus: continuationStatus,
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
