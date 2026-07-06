package control

import (
	"context"
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
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/token"
	"github.com/jackc/pgx/v5/pgtype"
)

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
		work.AfterCommit(func(ctx context.Context) {
			s.afterInputStreamRecordCommit(ctx, session, stream, &appended)
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
		var publicTokenPublicID string
		publicToken, err = createWithPublicID(r.Context(), []publicIDSlot{{prefix: publicid.PublicAccessToken, value: &publicTokenPublicID}}, func() (db.PublicAccessToken, error) {
			return work.q.CreatePublicAccessToken(r.Context(), db.CreatePublicAccessTokenParams{
				ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
				PublicID:      publicTokenPublicID,
				OrgID:         session.OrgID,
				ProjectID:     session.ProjectID,
				EnvironmentID: session.EnvironmentID,
				TokenHash:     tokenHash,
				ExpiresAt:     pgvalue.Timestamptz(expiresAt),
				MaxUses:       maxUses,
				Metadata:      []byte(`{}`),
				CreatedBy:     actorJSON(actor),
			})
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
	if err := s.requireRoutableRecordWorkerGroup(ctx, s.db, session.WorkerGroupID); err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, err
	}
	stream, err := s.ensureSessionStream(ctx, s.db, session, session.ActiveDeploymentID, streamName, direction)
	if err != nil {
		return db.Session{}, db.Stream{}, "", pgtype.Text{}, err
	}
	correlationID := strings.TrimSpace(scopeRequest.CorrelationID)
	return session, stream, scopeType, pgtype.Text{String: correlationID, Valid: correlationID != ""}, nil
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
		work.AfterCommit(func(ctx context.Context) {
			s.requeueResolvedRunWaits(ctx, token.OrgID)
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
	if err := s.requireRoutableRecordWorkerGroup(ctx, store, session.WorkerGroupID); err != nil {
		return db.Session{}, db.Stream{}, db.PublicAccessToken{}, err
	}
	stream, err := store.GetSessionStreamByName(ctx, db.GetSessionStreamByNameParams{
		OrgID:         publicToken.OrgID,
		WorkerGroupID: session.WorkerGroupID,
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
