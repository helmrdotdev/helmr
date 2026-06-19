package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicaccess"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	defaultControlPageSize           = int32(200)
	runDataJSONMaxBytes              = 256 * 1024
	channelRecordContentJSONMaxBytes = 1024 * 1024
	channelRecordRequestJSONMaxBytes = channelRecordContentJSONMaxBytes + 4096
	runScopedNameMaxBytes            = 256
	idempotencyKeyMaxBytes           = 512
)

func (s *Server) listRunWaitpoints(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		writeError(w, unavailable(errors.New("run storage is not configured")))
		return
	}
	runID, ok := s.authorizeRunAccess(w, r, auth.PermissionRunWaitpointsRead)
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
	status, err := optionalWaitpointStatusQuery(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if err := s.ensureRunWaitpointCursor(r.Context(), actorFromContext(r.Context()).OrgID, runID, cursorID); err != nil {
		if isNoRows(err) {
			writeError(w, gone(errors.New("run waitpoint cursor is no longer available")))
			return
		}
		s.log.Error("validate waitpoint cursor failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("validate waitpoint cursor"))
		return
	}
	rows, err := s.db.ListRunWaitpoints(r.Context(), db.ListRunWaitpointsParams{
		OrgID:      pgvalue.UUID(actorFromContext(r.Context()).OrgID),
		RunID:      pgvalue.UUID(runID),
		AfterID:    cursorID,
		LimitCount: limit + 1,
		Status:     status,
	})
	if err != nil {
		s.log.Error("list waitpoints failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("list waitpoints"))
		return
	}
	waitpoints := make([]api.PendingWaitpoint, 0, len(rows))
	rows, nextCursor := trimRunWaitpointsPage(rows, limit)
	for _, row := range rows {
		waitpoints = append(waitpoints, runWaitpointResponse(row))
	}
	writeJSON(w, http.StatusOK, api.ListRunWaitpointsResponse{Waitpoints: waitpoints, NextCursor: nextCursor})
}

func optionalWaitpointStatusQuery(r *http.Request) (pgtype.Text, error) {
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		return pgtype.Text{}, nil
	}
	switch status {
	case "pending", "completed", "timed_out", "cancelled", "failed":
		return pgvalue.Text(status), nil
	default:
		return pgtype.Text{}, fmt.Errorf("status must be one of pending, completed, timed_out, cancelled, failed")
	}
}

func writeChannelRecordsCORS(w http.ResponseWriter) {
	header := w.Header()
	header.Set("access-control-allow-origin", "*")
	header.Set("access-control-allow-methods", "GET, POST, OPTIONS")
	header.Set("access-control-allow-headers", strings.Join([]string{
		"authorization",
		"content-type",
		"idempotency-key",
		api.APIVersionHeader,
		api.SDKVersionHeader,
	}, ", "))
	header.Set("access-control-max-age", "600")
}

type sessionChannelPublicAccessTokenGrant struct {
	Channel       db.Channel
	Session       db.TaskSession
	ChannelName   string
	Direction     db.ChannelDirection
	ScopeType     string
	CorrelationID string
	ExpiresAt     time.Time
	MaxUses       pgtype.Int4
	CreatedBy     json.RawMessage
}

type sessionChannelPublicAccessTokenCreator interface {
	CreatePublicAccessToken(context.Context, db.CreatePublicAccessTokenParams) (db.PublicAccessToken, error)
}

func createSessionChannelPublicAccessToken(ctx context.Context, queries sessionChannelPublicAccessTokenCreator, authSecret []byte, grant sessionChannelPublicAccessTokenGrant) (string, db.PublicAccessToken, error) {
	binding, err := sessionChannelPublicAccessTokenBinding(grant)
	if err != nil {
		return "", db.PublicAccessToken{}, err
	}
	if !binding.TaskSessionID.Valid {
		return "", db.PublicAccessToken{}, badRequest(errors.New("channel must belong to a task session"))
	}
	if err := validateChannelPublicAccessScope(binding.Direction, grant.ScopeType); err != nil {
		return "", db.PublicAccessToken{}, badRequest(err)
	}
	expiresAt := grant.ExpiresAt
	if expiresAt.IsZero() {
		expiresAt = time.Now().Add(publicaccess.DefaultTokenTTL)
	}
	rawToken, tokenHash, err := publicaccess.NewToken(authSecret)
	if err != nil {
		return "", db.PublicAccessToken{}, err
	}
	sessionID := pgvalue.MustUUIDValue(binding.TaskSessionID).String()
	scope := publicAccessTokenScope{
		Type:          grant.ScopeType,
		SessionID:     sessionID,
		Channel:       binding.ChannelName,
		CorrelationID: strings.TrimSpace(grant.CorrelationID),
	}
	allowedScopes, err := json.Marshal([]publicAccessTokenScope{scope})
	if err != nil {
		return "", db.PublicAccessToken{}, err
	}
	metadata, err := json.Marshal(struct {
		Type          string `json:"type"`
		SessionID     string `json:"sessionId"`
		ChannelID     string `json:"channelId,omitempty"`
		Channel       string `json:"channel"`
		Direction     string `json:"direction"`
		CorrelationID string `json:"correlationId,omitempty"`
	}{
		Type:          "sessionChannel",
		SessionID:     sessionID,
		ChannelID:     optionalUUIDString(binding.ChannelID),
		Channel:       binding.ChannelName,
		Direction:     string(binding.Direction),
		CorrelationID: scope.CorrelationID,
	})
	if err != nil {
		return "", db.PublicAccessToken{}, err
	}
	createdBy := grant.CreatedBy
	if len(createdBy) == 0 {
		createdBy = json.RawMessage(`{"type":"control"}`)
	}
	row, err := queries.CreatePublicAccessToken(ctx, db.CreatePublicAccessTokenParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         binding.OrgID,
		ProjectID:     binding.ProjectID,
		EnvironmentID: binding.EnvironmentID,
		TokenHash:     tokenHash,
		AllowedScopes: allowedScopes,
		ExpiresAt:     pgvalue.Timestamptz(expiresAt),
		MaxUses:       grant.MaxUses,
		Metadata:      metadata,
		CreatedBy:     createdBy,
	})
	if err != nil {
		return "", db.PublicAccessToken{}, err
	}
	return rawToken, row, nil
}

type publicAccessTokenChannelBinding struct {
	OrgID         pgtype.UUID
	ProjectID     pgtype.UUID
	EnvironmentID pgtype.UUID
	TaskSessionID pgtype.UUID
	ChannelID     pgtype.UUID
	ChannelName   string
	Direction     db.ChannelDirection
}

func sessionChannelPublicAccessTokenBinding(grant sessionChannelPublicAccessTokenGrant) (publicAccessTokenChannelBinding, error) {
	if grant.Channel.ID.Valid {
		return publicAccessTokenChannelBinding{
			OrgID:         grant.Channel.OrgID,
			ProjectID:     grant.Channel.ProjectID,
			EnvironmentID: grant.Channel.EnvironmentID,
			TaskSessionID: grant.Channel.TaskSessionID,
			ChannelID:     grant.Channel.ID,
			ChannelName:   grant.Channel.Name,
			Direction:     grant.Channel.Direction,
		}, nil
	}
	if !grant.Session.ID.Valid {
		return publicAccessTokenChannelBinding{}, badRequest(errors.New("session is required"))
	}
	channelName := strings.TrimSpace(grant.ChannelName)
	if err := validateChannelName(channelName); err != nil {
		return publicAccessTokenChannelBinding{}, badRequest(err)
	}
	if grant.Direction != db.ChannelDirectionInput && grant.Direction != db.ChannelDirectionOutput {
		return publicAccessTokenChannelBinding{}, badRequest(errors.New("channel direction is required"))
	}
	return publicAccessTokenChannelBinding{
		OrgID:         grant.Session.OrgID,
		ProjectID:     grant.Session.ProjectID,
		EnvironmentID: grant.Session.EnvironmentID,
		TaskSessionID: grant.Session.ID,
		ChannelName:   channelName,
		Direction:     grant.Direction,
	}, nil
}

func optionalUUIDString(id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	return pgvalue.MustUUIDValue(id).String()
}

func validateChannelPublicAccessScope(direction db.ChannelDirection, scopeType string) error {
	switch scopeType {
	case "session.input.append":
		if direction != db.ChannelDirectionInput {
			return errors.New("session.input.append requires an input channel")
		}
	case "session.output.read":
		if direction != db.ChannelDirectionOutput {
			return errors.New("session.output.read requires an output channel")
		}
	default:
		return errors.New("unsupported channel public access scope")
	}
	return nil
}

func (s *Server) appendSessionChannelInputWithPublicToken(ctx context.Context, sessionID uuid.UUID, channelName string, tokenHash []byte, data []byte, correlationID string, idempotencyKey string, externalEventID string) (api.ChannelRecordResponse, string, error) {
	if s.tx == nil {
		return api.ChannelRecordResponse{}, "", errors.New("transactional channel storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	publicToken, err := queries.LockPublicAccessTokenByHash(ctx, tokenHash)
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	if !publicAccessTokenIsActive(publicToken, time.Now()) {
		return api.ChannelRecordResponse{}, "", pgx.ErrNoRows
	}
	channel, err := queries.GetTaskSessionChannelByName(ctx, db.GetTaskSessionChannelByNameParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		TaskSessionID: pgvalue.UUID(sessionID),
		Name:          channelName,
		Direction:     db.ChannelDirectionInput,
	})
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	if !publicAccessTokenAllowsChannel(publicToken.AllowedScopes, "session.input.append", channel, correlationID) {
		return api.ChannelRecordResponse{}, "", pgx.ErrNoRows
	}
	authSubjectID := pgvalue.MustUUIDValue(publicToken.ID).String()
	actor := []byte(`{"type":"public_access_token"}`)
	fingerprint, err := channelRecordFingerprint(channelRecordFingerprintInput{
		SessionID:       pgvalue.MustUUIDValue(channel.TaskSessionID).String(),
		ChannelID:       pgvalue.MustUUIDValue(channel.ID).String(),
		Channel:         channel.Name,
		Direction:       string(db.ChannelDirectionInput),
		ContentType:     "application/json",
		CorrelationID:   correlationID,
		Source:          "public_access_token",
		AuthSubjectType: "public_access_token",
		AuthSubjectID:   authSubjectID,
		ExternalEventID: externalEventID,
		Actor:           actor,
		Data:            data,
	})
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	var duplicateConflict error
	existing, found, err := findExistingPublicChannelRecord(ctx, queries, publicToken.OrgID, pgvalue.MustUUIDValue(channel.ID), idempotencyKey, externalEventID, correlationID, fingerprint)
	if err != nil {
		if errorStatus(err) != http.StatusConflict {
			return api.ChannelRecordResponse{}, "", err
		}
		duplicateConflict = err
	}
	if found && duplicateConflict == nil {
		// Idempotent retries of already-committed one-shot operations return
		// the original acknowledgement before max-use exhaustion is applied.
		if err := tx.Commit(ctx); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		return channelRecordDuplicateResponse(existing), "duplicate", nil
	}
	taskSession, err := queries.LockTaskSessionForChannelAppend(ctx, db.LockTaskSessionForChannelAppendParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		ChannelID:     channel.ID,
	})
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	if taskSession.Status != db.TaskSessionStatusOpen {
		if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
			OrgID: publicToken.OrgID,
			ID:    publicToken.ID,
		}); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		if duplicateConflict != nil {
			return api.ChannelRecordResponse{}, "", duplicateConflict
		}
		return api.ChannelRecordResponse{}, "", conflict(errors.New("session is terminal"))
	}
	duplicateConflict = nil
	existing, found, err = findExistingPublicChannelRecord(ctx, queries, publicToken.OrgID, pgvalue.MustUUIDValue(channel.ID), idempotencyKey, externalEventID, correlationID, fingerprint)
	if err != nil {
		if errorStatus(err) != http.StatusConflict {
			return api.ChannelRecordResponse{}, "", err
		}
		duplicateConflict = err
	}
	if found && duplicateConflict == nil {
		// Re-check after the session lock, preserving the same duplicate-before-
		// exhaustion contract while preventing concurrent non-duplicate appends.
		if err := tx.Commit(ctx); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		return channelRecordDuplicateResponse(existing), "duplicate", nil
	}
	if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	}); err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	if duplicateConflict != nil {
		if err := tx.Commit(ctx); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		return api.ChannelRecordResponse{}, "", duplicateConflict
	}
	record, err := queries.AppendSessionChannelInput(ctx, db.AppendSessionChannelInputParams{
		ID:                     pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                  publicToken.OrgID,
		RunID:                  taskSession.CurrentRunID,
		Channel:                channel.Name,
		Data:                   data,
		CorrelationID:          correlationID,
		ContentType:            "application/json",
		IdempotencyKey:         idempotencyKey,
		IdempotencyFingerprint: fingerprint,
		ExternalEventID:        externalEventID,
		AuthSubjectType:        "public_access_token",
		AuthSubjectID:          authSubjectID,
		MaxInputsPerChannel:    maxSessionInputsPerChannel,
	})
	if isNoRows(err) {
		conflictErr := s.sessionChannelInputAppendConflict(ctx, queries, publicToken.OrgID, taskSession.CurrentRunID, channel.Name, idempotencyKey, externalEventID, fingerprint)
		if err := tx.Commit(ctx); err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
		return api.ChannelRecordResponse{}, "", conflictErr
	}
	if err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	resumeRunIDs := []pgtype.UUID(nil)
	if record.Inserted {
		resumeRunIDs, err = resolveRunChannelWaitpointsWithQueries(ctx, queries, publicToken.OrgID, taskSession.CurrentRunID)
		if err != nil {
			return api.ChannelRecordResponse{}, "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return api.ChannelRecordResponse{}, "", err
	}
	if record.Inserted && taskSession.CurrentRunID.Valid && s.runEnqueuer != nil {
		if _, err := s.runEnqueuer.EnqueueRun(context.WithoutCancel(ctx), publicToken.OrgID, taskSession.CurrentRunID); err != nil {
			s.log.Error("enqueue session after public channel input failed", "session_id", sessionID.String(), "error", err)
		}
		for _, runID := range resumeRunIDs {
			if _, err := s.runEnqueuer.EnqueueRun(context.WithoutCancel(ctx), publicToken.OrgID, runID); err != nil {
				s.log.Error("enqueue dependent run after public channel input failed", "session_id", sessionID.String(), "run_id", pgvalue.MustUUIDValue(runID).String(), "error", err)
			}
		}
	}
	idempotencyStatus := "duplicate"
	if record.Inserted {
		idempotencyStatus = "created"
	}
	return appendSessionChannelInputResponse(record), idempotencyStatus, nil
}

type publicChannelRecordDuplicateReader interface {
	GetChannelRecordByIdempotencyKey(context.Context, db.GetChannelRecordByIdempotencyKeyParams) (db.ChannelRecord, error)
	GetChannelRecordByExternalEventID(context.Context, db.GetChannelRecordByExternalEventIDParams) (db.ChannelRecord, error)
}

func findExistingPublicChannelRecord(ctx context.Context, queries publicChannelRecordDuplicateReader, orgID pgtype.UUID, channelID uuid.UUID, idempotencyKey string, externalEventID string, correlationID string, fingerprint string) (db.ChannelRecord, bool, error) {
	if idempotencyKey != "" {
		existing, err := queries.GetChannelRecordByIdempotencyKey(ctx, db.GetChannelRecordByIdempotencyKeyParams{
			OrgID:          orgID,
			ChannelID:      pgvalue.UUID(channelID),
			IdempotencyKey: idempotencyKey,
		})
		if err == nil {
			return existing, true, ensureExistingChannelRecordMatches(existing, correlationID, fingerprint, "idempotency key")
		}
		if !isNoRows(err) {
			return db.ChannelRecord{}, false, err
		}
	}
	if externalEventID != "" {
		existing, err := queries.GetChannelRecordByExternalEventID(ctx, db.GetChannelRecordByExternalEventIDParams{
			OrgID:           orgID,
			ChannelID:       pgvalue.UUID(channelID),
			ExternalEventID: externalEventID,
		})
		if err == nil {
			return existing, true, ensureExistingChannelRecordMatches(existing, correlationID, fingerprint, "external event id")
		}
		if !isNoRows(err) {
			return db.ChannelRecord{}, false, err
		}
	}
	return db.ChannelRecord{}, false, nil
}

func ensureExistingChannelRecordMatches(existing db.ChannelRecord, correlationID string, fingerprint string, conflictSubject string) error {
	if existing.CorrelationID != correlationID {
		return conflict(fmt.Errorf("%s conflicts with a different channel record correlation", conflictSubject))
	}
	if existing.IdempotencyFingerprint != fingerprint {
		return conflict(fmt.Errorf("%s conflicts with a different channel record fingerprint", conflictSubject))
	}
	return nil
}

func (s *Server) listSessionChannelOutputsWithPublicToken(ctx context.Context, sessionID uuid.UUID, channelName string, tokenHash []byte, afterSequence int64, limit int32, correlationID string) ([]db.ChannelRecord, error) {
	if s.tx == nil {
		return nil, errors.New("transactional channel storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	publicToken, err := queries.LockPublicAccessTokenByHash(ctx, tokenHash)
	if err != nil {
		return nil, err
	}
	if !publicAccessTokenIsActive(publicToken, time.Now()) {
		return nil, pgx.ErrNoRows
	}
	session, err := queries.GetTaskSession(ctx, db.GetTaskSessionParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		ID:            pgvalue.UUID(sessionID),
	})
	if err != nil {
		return nil, err
	}
	if !publicAccessTokenAllowsSessionChannel(publicToken.AllowedScopes, "session.output.read", sessionID.String(), channelName, correlationID) {
		return nil, pgx.ErrNoRows
	}
	if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	}); err != nil {
		return nil, err
	}
	channel, err := queries.GetTaskSessionChannelByName(ctx, db.GetTaskSessionChannelByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		TaskSessionID: session.ID,
		Name:          channelName,
		Direction:     db.ChannelDirectionOutput,
	})
	if isNoRows(err) {
		if err := tx.Commit(ctx); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	records, err := queries.ListChannelRecords(ctx, db.ListChannelRecordsParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		ChannelID:     channel.ID,
		Direction:     db.ChannelDirectionOutput,
		AfterSequence: afterSequence,
		CorrelationID: optionalText(correlationID),
		LimitCount:    limit,
	})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return records, nil
}

func (s *Server) openSessionChannelOutputStreamWithPublicToken(ctx context.Context, sessionID uuid.UUID, channelName string, tokenHash []byte, correlationID string) (db.TaskSession, string, error) {
	if s.tx == nil {
		return db.TaskSession{}, "", errors.New("transactional channel storage is not configured")
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return db.TaskSession{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := db.New(tx)
	publicToken, err := queries.LockPublicAccessTokenByHash(ctx, tokenHash)
	if err != nil {
		return db.TaskSession{}, "", err
	}
	if !publicAccessTokenIsActive(publicToken, time.Now()) {
		return db.TaskSession{}, "", pgx.ErrNoRows
	}
	session, err := queries.GetTaskSession(ctx, db.GetTaskSessionParams{
		OrgID:         publicToken.OrgID,
		ProjectID:     publicToken.ProjectID,
		EnvironmentID: publicToken.EnvironmentID,
		ID:            pgvalue.UUID(sessionID),
	})
	if err != nil {
		return db.TaskSession{}, "", err
	}
	if !publicAccessTokenAllowsSessionChannel(publicToken.AllowedScopes, "session.output.read", sessionID.String(), channelName, correlationID) {
		return db.TaskSession{}, "", pgx.ErrNoRows
	}
	if _, err := queries.ConsumePublicAccessToken(ctx, db.ConsumePublicAccessTokenParams{
		OrgID: publicToken.OrgID,
		ID:    publicToken.ID,
	}); err != nil {
		return db.TaskSession{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return db.TaskSession{}, "", err
	}
	return session, channelName, nil
}

func publicAccessTokenAllowsChannel(raw []byte, scopeType string, channel db.Channel, correlationID string) bool {
	if !channel.TaskSessionID.Valid {
		return false
	}
	return publicAccessTokenAllowsSessionChannel(raw, scopeType, pgvalue.MustUUIDValue(channel.TaskSessionID).String(), channel.Name, correlationID)
}

func publicAccessTokenAllowsSessionChannel(raw []byte, scopeType string, sessionID string, channelName string, correlationID string) bool {
	var scopes []publicAccessTokenScope
	if err := json.Unmarshal(raw, &scopes); err != nil {
		return false
	}
	for _, scope := range scopes {
		if scope.Type != scopeType || scope.SessionID != sessionID || scope.Channel != channelName {
			continue
		}
		if scope.CorrelationID != "" && scope.CorrelationID != correlationID {
			continue
		}
		return true
	}
	return false
}

func publicAccessTokenIsActive(token db.PublicAccessToken, now time.Time) bool {
	if token.RevokedAt.Valid {
		return false
	}
	if !token.ExpiresAt.Valid {
		return false
	}
	return pgvalue.Time(token.ExpiresAt).After(now)
}

type channelRecordFingerprintInput struct {
	SessionID       string          `json:"session_id,omitempty"`
	ChannelID       string          `json:"channel_id,omitempty"`
	Channel         string          `json:"channel"`
	Direction       string          `json:"direction"`
	ContentType     string          `json:"content_type"`
	CorrelationID   string          `json:"correlation_id,omitempty"`
	Source          string          `json:"source"`
	AuthSubjectType string          `json:"auth_subject_type"`
	AuthSubjectID   string          `json:"auth_subject_id,omitempty"`
	ExternalEventID string          `json:"external_event_id,omitempty"`
	Actor           json.RawMessage `json:"actor"`
	Data            json.RawMessage `json:"data"`
}

func channelRecordFingerprint(input channelRecordFingerprintInput) (string, error) {
	if len(input.Data) == 0 {
		input.Data = []byte(`null`)
	}
	if len(input.Actor) == 0 {
		input.Actor = []byte(`{}`)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func validateChannelName(channel string) error {
	if channel == "" {
		return errors.New("channel is required")
	}
	if len([]byte(channel)) > runScopedNameMaxBytes {
		return fmt.Errorf("channel is %d bytes, exceeds max %d", len([]byte(channel)), runScopedNameMaxBytes)
	}
	for i, r := range channel {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if i > 0 && (r == '.' || r == '_' || r == '-') {
			continue
		}
		return errors.New("channel must start with a letter or number and may only contain letters, numbers, dots, underscores, and dashes")
	}
	return nil
}

type runChannelWaitpointResolver interface {
	ResolveRunChannelWaitpointsForRun(context.Context, db.ResolveRunChannelWaitpointsForRunParams) ([]db.ResolveRunChannelWaitpointsForRunRow, error)
	UnblockRunWaitpointsForWaitpoint(context.Context, db.UnblockRunWaitpointsForWaitpointParams) ([]db.UnblockRunWaitpointsForWaitpointRow, error)
}

func resolveRunChannelWaitpointsWithQueries(ctx context.Context, queries runChannelWaitpointResolver, orgID pgtype.UUID, runID pgtype.UUID) ([]pgtype.UUID, error) {
	runIDs := []pgtype.UUID(nil)
	for {
		completed, err := queries.ResolveRunChannelWaitpointsForRun(ctx, db.ResolveRunChannelWaitpointsForRunParams{
			OrgID: orgID,
			RunID: runID,
		})
		if err != nil {
			return nil, err
		}
		if len(completed) == 0 {
			return runIDs, nil
		}
		for _, waitpoint := range completed {
			resumed, err := queries.UnblockRunWaitpointsForWaitpoint(ctx, db.UnblockRunWaitpointsForWaitpointParams{
				OrgID:       orgID,
				WaitpointID: waitpoint.ID,
			})
			if err != nil {
				return nil, err
			}
			runIDs = append(runIDs, waitpointResumeRunIDs(resumed)...)
		}
	}
}

func (s *Server) workerWriteOutput(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerWriteOutputRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker output request JSON: %w", err)))
		return
	}
	channel := strings.TrimSpace(request.Channel)
	if err := validateChannelName(channel); err != nil {
		writeError(w, badRequest(err))
		return
	}
	payload := request.Payload
	if len(payload) == 0 {
		payload = []byte(`null`)
	}
	if len(payload) > runDataJSONMaxBytes {
		writeError(w, badRequest(fmt.Errorf("payload is %d bytes, exceeds max %d", len(payload), runDataJSONMaxBytes)))
		return
	}
	if !json.Valid(payload) {
		writeError(w, badRequest(errors.New("payload must be valid JSON")))
		return
	}
	if len(request.ObjectRef) > runDataJSONMaxBytes {
		writeError(w, badRequest(fmt.Errorf("object_ref is %d bytes, exceeds max %d", len(request.ObjectRef), runDataJSONMaxBytes)))
		return
	}
	if len(request.ObjectRef) > 0 && !json.Valid(request.ObjectRef) {
		writeError(w, badRequest(errors.New("object_ref must be valid JSON")))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	_, err := s.db.AppendExecutionChannelRecord(r.Context(), db.AppendExecutionChannelRecordParams{
		ID:               pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		Channel:          channel,
		Payload:          payload,
		ContentType:      request.ContentType,
		ObjectRef:        optionalJSON(request.ObjectRef),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if err != nil {
		s.log.Error("append worker output failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("append worker output"))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}

func (s *Server) workerUpdateRunMetadata(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerUpdateRunMetadataRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker metadata request JSON: %w", err)))
		return
	}
	operation := strings.TrimSpace(request.Operation)
	key := strings.TrimSpace(request.Key)
	value := request.Value
	patch := request.Patch
	switch operation {
	case "set":
		if key == "" {
			writeError(w, badRequest(errors.New("metadata key is required")))
			return
		}
		if err := validateRunMetadataKey(key); err != nil {
			writeError(w, badRequest(err))
			return
		}
		if len(value) == 0 {
			writeError(w, badRequest(errors.New("metadata value is required")))
			return
		}
		if len(value) > runDataJSONMaxBytes {
			writeError(w, badRequest(fmt.Errorf("metadata value is %d bytes, exceeds max %d", len(value), runDataJSONMaxBytes)))
			return
		}
		if !json.Valid(value) {
			writeError(w, badRequest(errors.New("metadata value must be valid JSON")))
			return
		}
		patch = []byte(`{}`)
	case "patch":
		if len(patch) == 0 {
			writeError(w, badRequest(errors.New("metadata patch is required")))
			return
		}
		normalized, err := normalizeRunMetadataPatchObject(patch)
		if err != nil {
			writeError(w, badRequest(err))
			return
		}
		patch = normalized
		value = []byte(`null`)
	case "increment":
		if key == "" {
			writeError(w, badRequest(errors.New("metadata key is required")))
			return
		}
		if err := validateRunMetadataKey(key); err != nil {
			writeError(w, badRequest(err))
			return
		}
		if math.IsNaN(request.Amount) || math.IsInf(request.Amount, 0) {
			writeError(w, badRequest(errors.New("metadata increment amount must be finite")))
			return
		}
		value = []byte(`null`)
		patch = []byte(`{}`)
	default:
		writeError(w, badRequest(fmt.Errorf("unsupported metadata operation %q", operation)))
		return
	}
	_, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	var amount pgtype.Numeric
	if err := amount.Scan(fmt.Sprintf("%g", request.Amount)); err != nil {
		writeError(w, badRequest(fmt.Errorf("metadata increment amount is invalid: %w", err)))
		return
	}
	updated, err := s.db.UpdateRunMetadataForExecution(r.Context(), db.UpdateRunMetadataForExecutionParams{
		OrgID:            pgvalue.UUID(leaseIDs.orgID),
		RunID:            pgvalue.UUID(leaseIDs.runID),
		RunLeaseID:       pgvalue.UUID(leaseIDs.runLeaseID),
		Operation:        operation,
		Key:              key,
		Value:            value,
		Patch:            patch,
		Amount:           amount,
		MaxMetadataBytes: int32(runDataJSONMaxBytes),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is stale")))
		return
	}
	if operation == "increment" && isPostgresNumericCastError(err) {
		writeError(w, badRequest(errors.New("metadata increment target must be numeric")))
		return
	}
	if err != nil {
		s.log.Error("update worker metadata failed", "run_id", request.Lease.RunID, "error", err)
		writeError(w, errors.New("update worker metadata"))
		return
	}
	if updated.MetadataTooLarge {
		writeError(w, badRequest(fmt.Errorf("run metadata exceeds max %d bytes", runDataJSONMaxBytes)))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerEventResponse{RunID: request.Lease.RunID})
}

func normalizeRunMetadataPatchObject(patch []byte) ([]byte, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, patch); err != nil {
		return nil, fmt.Errorf("metadata patch must be valid JSON: %w", err)
	}
	if compact.Len() > runDataJSONMaxBytes {
		return nil, fmt.Errorf("metadata patch is %d bytes, exceeds max %d", compact.Len(), runDataJSONMaxBytes)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(compact.Bytes(), &decoded); err != nil {
		return nil, errors.New("metadata patch must be a JSON object")
	}
	if decoded == nil {
		return nil, errors.New("metadata patch must be a JSON object")
	}
	for key := range decoded {
		if err := validateRunMetadataKey(key); err != nil {
			return nil, fmt.Errorf("metadata patch key %q is invalid: %w", key, err)
		}
	}
	return compact.Bytes(), nil
}

func validateRunMetadataKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("metadata key is required")
	}
	if len([]byte(key)) > runScopedNameMaxBytes {
		return fmt.Errorf("metadata key is %d bytes, exceeds max %d", len([]byte(key)), runScopedNameMaxBytes)
	}
	return nil
}

func isPostgresNumericCastError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "22P02", "22003", "22023":
		return true
	default:
		return false
	}
}

func (s *Server) authorizeRunAccess(w http.ResponseWriter, r *http.Request, permission auth.Permission) (uuid.UUID, bool) {
	runID, err := parseUUIDParam(r, "id")
	if err != nil {
		writeError(w, badRequest(err))
		return uuid.Nil, false
	}
	actor := actorFromContext(r.Context())
	run, err := s.db.GetRunSummary(r.Context(), db.GetRunSummaryParams{
		OrgID: pgvalue.UUID(actor.OrgID),
		ID:    pgvalue.UUID(runID),
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("run not found")))
		return uuid.Nil, false
	}
	if err != nil {
		s.log.Error("get run before run io failed", "run_id", runID.String(), "error", err)
		writeError(w, errors.New("get run"))
		return uuid.Nil, false
	}
	summary := getRunSummary(run)
	scope := auth.Scope{
		OrgID:         actor.OrgID,
		ProjectID:     pgvalue.MustUUIDValue(summary.ProjectID).String(),
		EnvironmentID: pgvalue.MustUUIDValue(summary.EnvironmentID).String(),
	}
	if err := s.requireActorScopeForRecord(r, actor, summary.ProjectID, summary.EnvironmentID); err != nil {
		if isNoRows(err) {
			writeError(w, notFound(errors.New("run not found")))
			return uuid.Nil, false
		}
		writeError(w, badRequest(err))
		return uuid.Nil, false
	}
	if !actor.HasPermission(permission, scope) {
		writeError(w, forbidden(errors.New("permission is required")))
		return uuid.Nil, false
	}
	return runID, true
}

func channelRecordResponse(row db.ChannelRecord) api.ChannelRecordResponse {
	return api.ChannelRecordResponse{
		ID:            pgvalue.MustUUIDValue(row.ID).String(),
		ChannelID:     pgvalue.MustUUIDValue(row.ChannelID).String(),
		Sequence:      row.Sequence,
		Data:          row.Data,
		CorrelationID: row.CorrelationID,
		ContentType:   row.ContentType,
		CreatedAt:     pgvalue.Time(row.CreatedAt),
	}
}

func channelRecordDuplicateResponse(row db.ChannelRecord) api.ChannelRecordResponse {
	return channelRecordResponse(row)
}

func runWaitpointResponse(row db.ListRunWaitpointsRow) api.PendingWaitpoint {
	response := api.PendingWaitpoint{
		ID:        pgvalue.MustUUIDValue(row.ID).String(),
		Kind:      string(row.Kind),
		Params:    row.Params,
		Metadata:  row.Metadata,
		Tags:      row.Tags,
		Status:    string(row.Status),
		CreatedAt: pgvalue.Time(row.CreatedAt),
	}
	if row.TimeoutSeconds.Valid {
		response.Timeout = &row.TimeoutSeconds.Int32
	}
	return response
}

func (s *Server) ensureRunWaitpointCursor(ctx context.Context, orgID uuid.UUID, runID uuid.UUID, cursorID pgtype.UUID) error {
	if !cursorID.Valid {
		return nil
	}
	_, err := s.db.GetRunWaitpointCursor(ctx, db.GetRunWaitpointCursorParams{
		OrgID:    pgvalue.UUID(orgID),
		RunID:    pgvalue.UUID(runID),
		CursorID: cursorID,
	})
	return err
}

func trimRunWaitpointsPage(rows []db.ListRunWaitpointsRow, limit int32) ([]db.ListRunWaitpointsRow, *string) {
	if int32(len(rows)) <= limit {
		return rows, nil
	}
	page := rows[:limit]
	next := pgvalue.MustUUIDValue(page[len(page)-1].ID).String()
	return page, &next
}

func optionalText(value string) pgtype.Text {
	value = strings.TrimSpace(value)
	if value == "" {
		return pgtype.Text{}
	}
	return pgtype.Text{String: value, Valid: true}
}

func optionalJSON(value json.RawMessage) []byte {
	if len(value) == 0 {
		return nil
	}
	return value
}

func optionalUUIDQuery(r *http.Request, key string) (pgtype.UUID, error) {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return pgtype.UUID{}, nil
	}
	parsed, err := uuid.Parse(value)
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%s must be a UUID", key)
	}
	return pgvalue.UUID(parsed), nil
}

func optionalLimitQuery(r *http.Request, fallback int32) (int32, error) {
	value := strings.TrimSpace(r.URL.Query().Get("limit"))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil || parsed < 1 || parsed > 1000 {
		return 0, errors.New("limit must be between 1 and 1000")
	}
	return int32(parsed), nil
}
