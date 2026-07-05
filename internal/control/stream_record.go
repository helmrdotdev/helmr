package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

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

func (s *Server) readOutputStreamRecord(ctx context.Context, store db.Querier, session db.Session, stream db.Stream, correlationID pgtype.Text, r *http.Request) (api.ReadStreamRecordResponse, error) {
	after, err := streamAfterSequence(r)
	if err != nil {
		return api.ReadStreamRecordResponse{}, badRequest(err)
	}
	records, err := store.ListStreamRecords(ctx, db.ListStreamRecordsParams{
		OrgID:         session.OrgID,
		CellID:        session.CellID,
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

func (s *Server) afterInputStreamRecordCommit(ctx context.Context, session db.Session, stream db.Stream, appended *appendedStreamRecord) {
	if appended == nil {
		return
	}
	s.publishSessionInputStreamWakeup(ctx, session.OrgID, stream.ID, appended.record.Sequence)
	if appended.resolvedWaitCount > 0 {
		s.requeueResolvedRunWaits(ctx, session.OrgID)
	}
	for _, runID := range s.sessionRunRequestWorkflow().reconcileAccepted(ctx, session.OrgID, session.ProjectID, session.EnvironmentID, session.ID) {
		appended.continuationRunID = runID
		appended.continuationStatus = "created"
	}
}

func (s *Server) appendStreamRecord(ctx context.Context, store db.Querier, session db.Session, stream db.Stream, direction db.StreamDirection, sourceType db.StreamRecordSourceType, sourceID string, publicAccessTokenID pgtype.UUID, request api.AppendStreamRecordRequest) (appendedStreamRecord, error) {
	if direction == db.StreamDirectionInput {
		locked, err := store.LockSession(ctx, db.LockSessionParams{
			OrgID:         session.OrgID,
			CellID:        session.CellID,
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
		CellID:                 session.CellID,
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
			CellID:        session.CellID,
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
				CellID:     session.CellID,
				LimitCount: int32(appended.resolvedWaitCount),
			}); err != nil {
				return appendedStreamRecord{}, err
			}
		}
		if !row.IsCached && appended.resolvedWaitCount == 0 {
			if _, err := store.EnsureSessionRunRequestForStreamRecord(ctx, db.EnsureSessionRunRequestForStreamRecordParams{
				ID:             pgvalue.UUID(uuid.Must(uuid.NewV7())),
				OrgID:          session.OrgID,
				CellID:         session.CellID,
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
		CellID:        session.CellID,
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
