package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	waitpointResponseTokenPrefix     = "hlmr_wpt_"
	waitpointResponseTokenBytes      = 32
	defaultWaitpointResponseTokenTTL = 24 * time.Hour
)

func (s *Server) createWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("token hashing is not configured"))
		return
	}
	var request api.CreateWaitpointTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid waitpoint token request JSON: %w", err))
		return
	}
	runID, err := ids.Parse(request.RunID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("run_id must be a UUID"))
		return
	}
	waitpointID, err := ids.Parse(request.WaitpointID)
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("waitpoint_id must be a UUID"))
		return
	}
	now := time.Now().UTC()
	expiresAt, err := waitpointTokenExpiry(now, request.ExpiresAt, request.ExpiresInSeconds, defaultWaitpointResponseTokenTTL, "expires")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	metadata, err := normalizeWaitpointTokenMetadata(request.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: ids.ToPG(actor.OrgID),
		ID:    ids.ToPG(runID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		s.log.Error("get run before creating waitpoint token failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	scope, err := s.runScope(r.Context(), actor.OrgID, getRunSummary(run))
	if err != nil {
		s.log.Error("resolve run scope before creating waitpoint token failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	waitpoint, err := s.db.GetWaitpointForResponseTokenCreation(r.Context(), db.GetWaitpointForResponseTokenCreationParams{
		OrgID:       ids.ToPG(actor.OrgID),
		RunID:       ids.ToPG(runID),
		WaitpointID: ids.ToPG(waitpointID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint before creating waitpoint token failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	if waitpoint.Kind == db.WaitpointKindDelay {
		writeError(w, http.StatusBadRequest, errors.New("delay waitpoints cannot be completed externally"))
		return
	}
	rawToken, tokenHash, err := s.generateWaitpointResponseToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("generate waitpoint token"))
		return
	}
	row, err := s.db.CreateWaitpointResponseToken(r.Context(), db.CreateWaitpointResponseTokenParams{
		ID:              ids.ToPG(ids.New()),
		OrgID:           ids.ToPG(actor.OrgID),
		RunID:           ids.ToPG(runID),
		WaitpointID:     ids.ToPG(waitpointID),
		TokenHash:       tokenHash,
		ExpiresAt:       pgTimeToPG(expiresAt),
		ExternalSubject: pgtype.Text{},
		Metadata:        metadata,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
		return
	}
	if err != nil {
		s.log.Error("create waitpoint token failed", "run_id", runID.String(), "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	writeJSON(w, http.StatusCreated, s.waitpointTokenResponseFromCreate(row, rawToken))
}

func (s *Server) completeWaitpointToken(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("run storage is not configured"))
		return
	}
	if err := auth.ValidateTokenSecret(s.authSecret); err != nil {
		writeError(w, http.StatusServiceUnavailable, errors.New("token hashing is not configured"))
		return
	}
	tokenID, err := ids.Parse(chi.URLParam(r, "tokenID"))
	if err != nil {
		writeError(w, http.StatusBadRequest, errors.New("tokenID must be a UUID"))
		return
	}
	request, err := decodeCompleteWaitpointTokenRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rawToken := strings.TrimSpace(request.Token)
	if rawToken == "" {
		if bearer, ok := bearerToken(r.Header.Get("authorization")); ok {
			rawToken = bearer
		}
	}
	tokenHash, err := s.hashWaitpointResponseToken(rawToken)
	if err != nil {
		writeError(w, http.StatusUnauthorized, errors.New("invalid token"))
		return
	}
	token, err := s.db.GetActiveWaitpointResponseToken(r.Context(), db.GetActiveWaitpointResponseTokenParams{
		ID:        ids.ToPG(tokenID),
		TokenHash: tokenHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("invalid or inactive token"))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("complete waitpoint token"))
		return
	}
	if token.WaitpointKind == db.WaitpointKindDelay {
		writeError(w, http.StatusConflict, errors.New("delay waitpoints cannot be completed externally"))
		return
	}
	completionMetadata, err := normalizeWaitpointTokenMetadata(request.Metadata)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	externalSubject := waitpointTokenCompletionSubject(token, request.ExternalSubject)
	principal := waitpointTokenPrincipal(token, externalSubject)
	now := time.Now().UTC()
	resolutionKind, outputPayload, resolutionPayload, eventPayload, err := tokenWaitpointResolution(principal, request.Value, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	eventPayload["kind"] = string(token.WaitpointKind)
	eventPayload["run_id"] = ids.MustFromPG(token.RunID).String()
	eventPayload["waitpoint_id"] = ids.MustFromPG(token.WaitpointID).String()
	eventJSON, err := json.Marshal(eventPayload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode waitpoint resolved event"))
		return
	}
	completeParams := db.CompleteWaitpointResponseTokenParams{
		ID:                   ids.ToPG(tokenID),
		TokenHash:            tokenHash,
		Action:               string(api.WaitpointTokenActionComplete),
		Kind:                 token.WaitpointKind,
		ResponseID:           ids.ToPG(ids.New()),
		ResponseKey:          "token:" + tokenID.String(),
		ResolutionKind:       pgtype.Text{String: resolutionKind, Valid: true},
		Resolution:           resolutionPayload,
		EventPayload:         eventJSON,
		CompletedByPrincipal: pgtype.Text{String: principal, Valid: true},
		CompletedVia:         pgtype.Text{String: "waitpoint_response_token", Valid: true},
		ExternalSubject:      pgText(externalSubject),
		Metadata:             completionMetadata,
	}
	resolveParams := db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: resolutionKind, Valid: true},
		Output:         outputPayload,
		Resolution:     resolutionPayload,
		OrgID:          token.OrgID,
		RunID:          token.RunID,
		ID:             token.WaitpointID,
		Kind:           token.WaitpointKind,
		Payload:        eventJSON,
	}
	outcome, err := s.completeAndResolveWaitpointToken(r.Context(), completeParams, resolveParams)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("waitpoint token cannot complete this waitpoint"))
		return
	}
	if err != nil {
		s.log.Error("complete waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("complete waitpoint token"))
		return
	}
	if acceptsHTML(r) {
		status := http.StatusOK
		body := "<p>Your waitpoint response was recorded.</p>"
		if !outcome.Resumed {
			status = http.StatusAccepted
			body = "<p>Your waitpoint response was recorded. The run will resume after enough responses are collected.</p>"
		}
		writeWaitpointHTML(w, status, "Response recorded", body)
		return
	}
	if !outcome.Resumed {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeAndResolveWaitpointToken(ctx context.Context, completeParams db.CompleteWaitpointResponseTokenParams, resolveParams db.ResolveWaitpointParams) (waitpointResolveOutcome, error) {
	if s.tx == nil {
		if store, ok := s.db.(interface {
			CompleteAndResolveWaitpointToken(context.Context, db.CompleteWaitpointResponseTokenParams, db.ResolveWaitpointParams) (db.ResolveWaitpointRow, error)
		}); ok {
			resolved, err := store.CompleteAndResolveWaitpointToken(ctx, completeParams, resolveParams)
			if err != nil {
				return waitpointResolveOutcome{}, err
			}
			return waitpointResolveOutcomeFromStatus(resolved.Status), nil
		}
		return waitpointResolveOutcome{}, errors.New("transactional waitpoint storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	if _, err := queries.CompleteWaitpointResponseToken(ctx, completeParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	resolved, err := queries.ResolveWaitpoint(ctx, resolveParams)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcomeFromStatus(resolved.Status), nil
}

func decodeCompleteWaitpointTokenRequest(r *http.Request) (api.CompleteWaitpointTokenRequest, error) {
	if strings.Contains(r.Header.Get("content-type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return api.CompleteWaitpointTokenRequest{}, fmt.Errorf("invalid waitpoint token completion form: %w", err)
		}
		return api.CompleteWaitpointTokenRequest{
			Token: strings.TrimSpace(r.Form.Get("token")),
			Value: json.RawMessage(strings.TrimSpace(r.Form.Get("value"))),
		}, nil
	}
	var request api.CompleteWaitpointTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		return api.CompleteWaitpointTokenRequest{}, fmt.Errorf("invalid waitpoint token completion JSON: %w", err)
	}
	return request, nil
}

func (s *Server) generateWaitpointResponseToken() (string, []byte, error) {
	raw, err := auth.GenerateOpaqueToken(waitpointResponseTokenBytes)
	if err != nil {
		return "", nil, err
	}
	token := waitpointResponseTokenPrefix + raw
	hash, err := s.hashWaitpointResponseToken(token)
	if err != nil {
		return "", nil, err
	}
	return token, hash, nil
}

func (s *Server) hashWaitpointResponseToken(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, waitpointResponseTokenPrefix) {
		return nil, auth.ErrUnauthenticated
	}
	return auth.HashToken(s.authSecret, raw)
}

func waitpointTokenExpiry(now time.Time, expiresAt *time.Time, expiresInSeconds *int32, defaultTTL time.Duration, name string) (time.Time, error) {
	if expiresAt != nil && expiresInSeconds != nil {
		return time.Time{}, fmt.Errorf("%s_at and %s_in_seconds cannot both be set", name, name)
	}
	if expiresAt != nil {
		expiry := expiresAt.UTC()
		if !expiry.After(now) {
			return time.Time{}, fmt.Errorf("%s_at must be in the future", name)
		}
		return expiry, nil
	}
	if expiresInSeconds != nil {
		if *expiresInSeconds <= 0 {
			return time.Time{}, fmt.Errorf("%s_in_seconds must be positive", name)
		}
		return now.Add(time.Duration(*expiresInSeconds) * time.Second), nil
	}
	return now.Add(defaultTTL), nil
}

func normalizeWaitpointTokenMetadata(metadata json.RawMessage) ([]byte, error) {
	if len(metadata) == 0 {
		return []byte(`{}`), nil
	}
	var object map[string]any
	if err := json.Unmarshal(metadata, &object); err != nil {
		return nil, fmt.Errorf("metadata must be a JSON object: %w", err)
	}
	if object == nil {
		return nil, errors.New("metadata must be a JSON object")
	}
	return metadata, nil
}

func (s *Server) waitpointTokenResponseFromCreate(row db.WaitpointResponseToken, rawToken string) api.WaitpointTokenResponse {
	expiresAt := pgTime(row.ExpiresAt)
	var expiresAtPtr *time.Time
	if row.ExpiresAt.Valid {
		expiresAtPtr = &expiresAt
	}
	return api.WaitpointTokenResponse{
		ID:          ids.MustFromPG(row.ID).String(),
		RunID:       ids.MustFromPG(row.RunID).String(),
		WaitpointID: ids.MustFromPG(row.WaitpointID).String(),
		URL:         s.waitpointTokenURL(ids.MustFromPG(row.ID).String(), rawToken),
		Token:       rawToken,
		ExpiresAt:   expiresAtPtr,
	}
}

func waitpointTokenCompletionSubject(token db.GetActiveWaitpointResponseTokenRow, requested string) string {
	if subject := strings.TrimSpace(token.ExternalSubject.String); token.ExternalSubject.Valid && subject != "" {
		return subject
	}
	return strings.TrimSpace(requested)
}

func waitpointTokenPrincipal(token db.GetActiveWaitpointResponseTokenRow, externalSubject string) string {
	if externalSubject != "" {
		return externalSubject
	}
	var metadata map[string]any
	if err := json.Unmarshal(token.Metadata, &metadata); err == nil {
		for _, key := range []string{"principal", "external_subject", "subject"} {
			if value, ok := metadata[key].(string); ok {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return "external"
}

func tokenWaitpointResolution(principal string, value json.RawMessage, now time.Time) (string, []byte, []byte, map[string]any, error) {
	if len(value) == 0 {
		value = []byte("null")
	}
	if !json.Valid(value) {
		return "", nil, nil, nil, errors.New("value must be valid JSON")
	}
	output := append([]byte(nil), value...)
	payload, err := json.Marshal(map[string]any{
		"value":     json.RawMessage(value),
		"principal": principal,
		"at":        now.Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", nil, nil, nil, err
	}
	eventPayload := map[string]any{
		"kind":            "token",
		"resolution_kind": "completed",
		"result":          json.RawMessage(value),
	}
	return "completed", output, payload, eventPayload, nil
}

func (s *Server) waitpointTokenURL(id string, token string) string {
	confirmation, err := s.waitpointConfirmationURL(id, token)
	if err == nil {
		return confirmation
	}
	return waitpointConfirmationPath(id, token)
}

func pgText(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}
