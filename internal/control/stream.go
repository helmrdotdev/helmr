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
		CellID:        session.CellID,
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
			CellID:             session.CellID,
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

func (s *Server) publicSessionInputStreamPreflight(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessPreflight(w, r, "POST")
}

func (s *Server) publicSessionOutputStreamReadPreflight(w http.ResponseWriter, r *http.Request) {
	s.writeBrowserPublicAccessPreflight(w, r, "GET")
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

func parseUUIDString(raw string, label string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(raw))
	if err != nil {
		return uuid.Nil, fmt.Errorf("%s must be a UUID", label)
	}
	return id, nil
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
