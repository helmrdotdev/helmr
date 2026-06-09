package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type waitpointResponseData struct {
	ResolutionKind string
	Output         []byte
	Resolution     []byte
	EventPayload   map[string]any
	Metadata       []byte
}

func waitpointKindExternallyCompletable(kind db.WaitpointKind) bool {
	switch kind {
	case db.WaitpointKindHuman:
		return true
	default:
		return false
	}
}

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
	waitpoint, err := s.db.GetWaitpointForResponseTokenCreation(r.Context(), db.GetWaitpointForResponseTokenCreationParams{
		OrgID:       ids.ToPG(actor.OrgID),
		WaitpointID: ids.ToPG(waitpointID),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("pending waitpoint not found"))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint before creating waitpoint token failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     ids.MustFromPG(waitpoint.ProjectID).String(),
		EnvironmentID: ids.MustFromPG(waitpoint.EnvironmentID).String(),
	}
	if !actor.HasPermission(auth.PermissionWaitpointsRespond, scope) {
		writeError(w, http.StatusForbidden, errors.New("permission is required"))
		return
	}
	if !waitpointKindExternallyCompletable(waitpoint.Kind) {
		writeError(w, http.StatusBadRequest, errors.New("waitpoint kind cannot be responded to externally"))
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
		s.log.Error("create waitpoint token failed", "waitpoint_id", waitpointID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("create waitpoint token"))
		return
	}
	writeJSON(w, http.StatusCreated, s.waitpointTokenResponseFromCreate(row, rawToken))
}

func (s *Server) respondWaitpointToken(w http.ResponseWriter, r *http.Request) {
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
	request, err := decodeRespondWaitpointRequest(r)
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
	token, err := s.db.GetWaitpointResponseTokenForRespond(r.Context(), db.GetWaitpointResponseTokenForRespondParams{
		ID:        ids.ToPG(tokenID),
		TokenHash: tokenHash,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, errors.New("invalid or inactive token"))
		return
	}
	if err != nil {
		s.log.Error("get waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("respond with waitpoint token"))
		return
	}
	if !waitpointKindExternallyCompletable(token.WaitpointKind) {
		writeError(w, http.StatusConflict, errors.New("waitpoint kind cannot be responded to externally"))
		return
	}
	externalSubject := waitpointTokenResponseSubject(token, request.ExternalSubject)
	principal := waitpointTokenPrincipal(token, externalSubject)
	response, err := waitpointResponsePayload(token.WaitpointKind, principal, request.Value, request.Metadata, time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	response.EventPayload["waitpoint_id"] = ids.MustFromPG(token.WaitpointID).String()
	eventJSON, err := json.Marshal(response.EventPayload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, errors.New("encode waitpoint resolved event"))
		return
	}
	recordParams := db.RecordWaitpointResponseParams{
		ID:                   ids.ToPG(ids.New()),
		OrgID:                token.OrgID,
		WaitpointID:          token.WaitpointID,
		ResponseKey:          "token:" + tokenID.String(),
		RequestHash:          waitpointResponseRequestHash(request.Value, request.ExternalSubject, request.Metadata),
		Action:               "respond",
		Kind:                 token.WaitpointKind,
		ResolutionKind:       pgtype.Text{String: response.ResolutionKind, Valid: true},
		Resolution:           response.Resolution,
		EventPayload:         eventJSON,
		CompletedByPrincipal: pgtype.Text{String: principal, Valid: true},
		CompletedVia:         pgtype.Text{String: "waitpoint_response_token", Valid: true},
		ExternalSubject:      pgText(externalSubject),
		Metadata:             response.Metadata,
	}
	resolveParams := db.ResolveWaitpointParams{
		ResolutionKind: pgtype.Text{String: response.ResolutionKind, Valid: true},
		Output:         response.Output,
		Resolution:     response.Resolution,
		OrgID:          token.OrgID,
		ID:             token.WaitpointID,
		Kind:           token.WaitpointKind,
	}
	markParams := db.MarkWaitpointResponseTokenCompletedParams{
		OrgID:                token.OrgID,
		ID:                   ids.ToPG(tokenID),
		TokenHash:            tokenHash,
		CompletedByPrincipal: pgtype.Text{String: principal, Valid: true},
		CompletedVia:         pgtype.Text{String: "waitpoint_response_token", Valid: true},
		ExternalSubject:      pgText(externalSubject),
		Metadata:             response.Metadata,
	}
	outcome, err := s.respondWithWaitpointToken(r.Context(), markParams, recordParams, resolveParams)
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusConflict, errors.New("waitpoint token cannot resolve this waitpoint"))
		return
	}
	if err != nil {
		s.log.Error("respond with waitpoint token failed", "token_id", tokenID.String(), "error", err)
		writeError(w, http.StatusInternalServerError, errors.New("respond with waitpoint token"))
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

func (s *Server) respondWithWaitpointToken(ctx context.Context, markParams db.MarkWaitpointResponseTokenCompletedParams, recordParams db.RecordWaitpointResponseParams, resolveParams db.ResolveWaitpointParams) (waitpointResolveOutcome, error) {
	if s.tx == nil {
		if store, ok := s.db.(interface {
			RespondWithWaitpointToken(context.Context, db.MarkWaitpointResponseTokenCompletedParams, db.RecordWaitpointResponseParams, db.ResolveWaitpointParams) ([]db.UnblockRunWaitsForWaitpointRow, error)
		}); ok {
			resumed, err := store.RespondWithWaitpointToken(ctx, markParams, recordParams, resolveParams)
			if err != nil {
				return waitpointResolveOutcome{}, err
			}
			return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
		}
		return waitpointResolveOutcome{}, errors.New("transactional waitpoint storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	if _, err := queries.GetWaitpointResponseTokenForRespond(ctx, db.GetWaitpointResponseTokenForRespondParams{
		ID:        markParams.ID,
		TokenHash: markParams.TokenHash,
	}); err != nil {
		return waitpointResolveOutcome{}, err
	}
	if _, err := queries.MarkWaitpointResponseTokenCompleted(ctx, markParams); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return waitpointResolveOutcome{}, err
	}
	if _, err := queries.RecordWaitpointResponse(ctx, recordParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	if _, err := queries.ResolveWaitpoint(ctx, resolveParams); err != nil {
		return waitpointResolveOutcome{}, err
	}
	resumed, err := queries.UnblockRunWaitsForWaitpoint(ctx, db.UnblockRunWaitsForWaitpointParams{
		OrgID:       resolveParams.OrgID,
		WaitpointID: resolveParams.ID,
	})
	if err != nil {
		return waitpointResolveOutcome{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return waitpointResolveOutcome{}, err
	}
	return waitpointResolveOutcome{Resumed: len(resumed) > 0}, nil
}

func decodeRespondWaitpointRequest(r *http.Request) (api.RespondWaitpointRequest, error) {
	if strings.Contains(r.Header.Get("content-type"), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			return api.RespondWaitpointRequest{}, fmt.Errorf("invalid waitpoint token response form: %w", err)
		}
		value, err := waitpointFormValue(r.Form.Get("value"))
		if err != nil {
			return api.RespondWaitpointRequest{}, err
		}
		return api.RespondWaitpointRequest{
			Token: strings.TrimSpace(r.Form.Get("token")),
			Value: value,
		}, nil
	}
	var request api.RespondWaitpointRequest
	if err := decodeJSON(r, &request); err != nil {
		return api.RespondWaitpointRequest{}, fmt.Errorf("invalid waitpoint token response JSON: %w", err)
	}
	return request, nil
}

func waitpointFormValue(raw string) (json.RawMessage, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return json.RawMessage("null"), nil
	}
	if json.Valid([]byte(value)) {
		return json.RawMessage(value), nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode waitpoint form value: %w", err)
	}
	return json.RawMessage(encoded), nil
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
		WaitpointID: ids.MustFromPG(row.WaitpointID).String(),
		URL:         s.waitpointTokenURL(ids.MustFromPG(row.ID).String(), rawToken),
		Token:       rawToken,
		ExpiresAt:   expiresAtPtr,
	}
}

func waitpointTokenResponseSubject(token db.GetWaitpointResponseTokenForRespondRow, requested string) string {
	if subject := strings.TrimSpace(token.ExternalSubject.String); token.ExternalSubject.Valid && subject != "" {
		return subject
	}
	return strings.TrimSpace(requested)
}

func waitpointTokenPrincipal(token db.GetWaitpointResponseTokenForRespondRow, externalSubject string) string {
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

func waitpointResponsePayload(kind db.WaitpointKind, principal string, value json.RawMessage, metadata json.RawMessage, now time.Time) (waitpointResponseData, error) {
	if !waitpointKindExternallyCompletable(kind) {
		return waitpointResponseData{}, errors.New("waitpoint kind cannot be responded to externally")
	}
	responseMetadata, err := normalizeWaitpointTokenMetadata(metadata)
	if err != nil {
		return waitpointResponseData{}, err
	}
	resolutionKind, output, resolution, eventPayload, err := humanWaitpointResolution(kind, principal, value, now)
	if err != nil {
		return waitpointResponseData{}, err
	}
	return waitpointResponseData{
		ResolutionKind: resolutionKind,
		Output:         output,
		Resolution:     resolution,
		EventPayload:   eventPayload,
		Metadata:       responseMetadata,
	}, nil
}

func humanWaitpointResolution(kind db.WaitpointKind, principal string, value json.RawMessage, now time.Time) (string, []byte, []byte, map[string]any, error) {
	if len(value) == 0 {
		value = []byte("null")
	}
	if !json.Valid(value) {
		return "", nil, nil, nil, errors.New("value must be valid JSON")
	}
	payload, err := json.Marshal(map[string]any{
		"value":     json.RawMessage(value),
		"principal": principal,
		"at":        now.Format(time.RFC3339Nano),
	})
	if err != nil {
		return "", nil, nil, nil, err
	}
	eventPayload := map[string]any{
		"kind":            string(kind),
		"resolution_kind": "completed",
		"result":          json.RawMessage(value),
	}
	return "completed", value, payload, eventPayload, nil
}

func waitpointResponseRequestHash(value json.RawMessage, _ string, metadata json.RawMessage) string {
	if len(value) == 0 {
		value = []byte("null")
	}
	if len(metadata) == 0 {
		metadata = []byte("{}")
	}
	payload, _ := json.Marshal(map[string]any{
		"value":    json.RawMessage(value),
		"metadata": json.RawMessage(metadata),
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
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
