package control

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	activeStreamReadDefaultTimeout = 30 * time.Minute
	activeStreamReadMaxTimeout     = 30 * time.Minute
	activeStreamWakeupMaxLen       = int64(10000)
	activeStreamWakeupBlockEvery   = 25 * time.Second
)

var (
	errActiveStreamUnavailable = codedError{code: "active_stream_unavailable", message: "active stream transport unavailable"}
	errActiveStreamLeaseLost   = codedError{code: "active_stream_lease_lost", message: "active stream run lease is no longer active"}
)

func (s *Server) workerAppendOutputStream(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerOutputStreamAppendRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker output stream append request JSON: %w", err)))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	leaseRow, _, err := s.workerExecutionLease(r.Context(), worker, leaseIDs)
	if isNoRows(err) {
		writeError(w, conflict(errActiveStreamLeaseLost))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker stream lease"))
		return
	}
	session, stream, err := s.workerStreamScope(r.Context(), leaseIDs, queueLeaseStreamScope(leaseRow), request.Stream, db.StreamDirectionOutput)
	if err != nil {
		s.writeWorkerStreamError(w, err)
		return
	}
	appended, err := s.appendStreamRecord(r.Context(), s.db, session, stream, db.StreamDirectionOutput, db.StreamRecordSourceTypeWorkerLease, leaseIDs.runLeaseID.String(), pgtype.UUID{}, api.AppendStreamRecordRequest{
		Data:           request.Data,
		ContentType:    request.ContentType,
		CorrelationID:  request.CorrelationID,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		s.writeWorkerStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, appendStreamRecordResponse(appended.record, ""))
}

func (s *Server) workerReadInputStream(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerActiveStreamReadRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker input stream read request JSON: %w", err)))
		return
	}
	if request.AfterSequence < 0 {
		writeError(w, badRequest(errors.New("after_sequence must be non-negative")))
		return
	}
	worker, leaseIDs, ok := s.workerRunLeaseForWrite(w, r, request.Lease)
	if !ok {
		return
	}
	response, err := s.readWorkerInputStream(r.Context(), worker, leaseIDs, request)
	if err != nil {
		s.writeWorkerStreamError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) readWorkerInputStream(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs, request api.WorkerActiveStreamReadRequest) (api.WorkerActiveStreamReadResponse, error) {
	return s.readWorkerInputStreamWithWakeups(ctx, worker, leaseIDs, request, serverActiveStreamWakeups{server: s})
}

type activeStreamWakeups interface {
	latestSessionInputStreamWakeupID(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID) (string, error)
	waitSessionInputStreamWakeup(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID, cursor string, maxWait time.Duration) (string, error)
}

type serverActiveStreamWakeups struct {
	server *Server
}

func (w serverActiveStreamWakeups) latestSessionInputStreamWakeupID(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID) (string, error) {
	return w.server.latestSessionInputStreamWakeupID(ctx, orgID, streamID)
}

func (w serverActiveStreamWakeups) waitSessionInputStreamWakeup(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID, cursor string, maxWait time.Duration) (string, error) {
	return w.server.waitSessionInputStreamWakeup(ctx, orgID, streamID, cursor, maxWait)
}

func (s *Server) readWorkerInputStreamWithWakeups(ctx context.Context, worker workerActor, leaseIDs workerRunLeaseIDs, request api.WorkerActiveStreamReadRequest, wakeups activeStreamWakeups) (api.WorkerActiveStreamReadResponse, error) {
	leaseRow, err := s.workerCurrentRunningLease(ctx, worker, leaseIDs)
	if isNoRows(err) {
		return api.WorkerActiveStreamReadResponse{}, conflict(errActiveStreamLeaseLost)
	}
	if err != nil {
		return api.WorkerActiveStreamReadResponse{}, err
	}
	session, stream, err := s.workerStreamScope(ctx, leaseIDs, currentLeaseStreamScope(leaseRow), request.Stream, db.StreamDirectionInput)
	if err != nil {
		return api.WorkerActiveStreamReadResponse{}, err
	}
	deadline := time.Now()
	if request.Block {
		deadline = deadline.Add(workerActiveStreamReadTimeout(request.TimeoutSeconds))
	}
	cursor := ""
	cursorReady := false
	leaseValidated := true
	for {
		if !leaseValidated {
			if _, err := s.workerCurrentRunningLease(ctx, worker, leaseIDs); isNoRows(err) {
				return api.WorkerActiveStreamReadResponse{}, conflict(errActiveStreamLeaseLost)
			} else if err != nil {
				return api.WorkerActiveStreamReadResponse{}, err
			}
			leaseValidated = true
		}
		record, found, err := s.readInputStreamRecord(ctx, s.db, session, stream, request.AfterSequence, strings.TrimSpace(request.CorrelationID))
		if err != nil {
			return api.WorkerActiveStreamReadResponse{}, err
		}
		if found {
			if err := s.sessionContinuationRequestWorkflow().consumeByActiveRun(ctx, session, pgvalue.UUID(leaseIDs.runID), record.ID); err != nil {
				return api.WorkerActiveStreamReadResponse{}, err
			}
			response := streamRecordResponse(record)
			return api.WorkerActiveStreamReadResponse{Record: &response}, nil
		}
		if !request.Block || !time.Now().Before(deadline) {
			return api.WorkerActiveStreamReadResponse{TimedOut: true}, nil
		}
		if !cursorReady {
			cursor, err = wakeups.latestSessionInputStreamWakeupID(ctx, session.OrgID, stream.ID)
			if err != nil {
				return api.WorkerActiveStreamReadResponse{}, unavailable(errActiveStreamUnavailable)
			}
			cursorReady = true
			continue
		}
		cursor, err = wakeups.waitSessionInputStreamWakeup(ctx, session.OrgID, stream.ID, cursor, time.Until(deadline))
		if err != nil {
			return api.WorkerActiveStreamReadResponse{}, unavailable(errActiveStreamUnavailable)
		}
		leaseValidated = false
	}
}

type workerRunLeaseStreamScope struct {
	workerGroupID string
	projectID     pgtype.UUID
	environmentID pgtype.UUID
	deploymentID  pgtype.UUID
	taskID        string
	sessionID     pgtype.UUID
}

func queueLeaseStreamScope(row db.GetRunLeaseQueueLeaseRow) workerRunLeaseStreamScope {
	return workerRunLeaseStreamScope{workerGroupID: row.WorkerGroupID, projectID: row.ProjectID, environmentID: row.EnvironmentID, deploymentID: row.DeploymentID, taskID: row.TaskID, sessionID: row.SessionID}
}

func currentLeaseStreamScope(row db.GetCurrentRunningRunLeaseRow) workerRunLeaseStreamScope {
	return workerRunLeaseStreamScope{workerGroupID: row.WorkerGroupID, projectID: row.ProjectID, environmentID: row.EnvironmentID, deploymentID: row.DeploymentID, taskID: row.TaskID, sessionID: row.SessionID}
}

func (s *Server) workerStreamScope(ctx context.Context, leaseIDs workerRunLeaseIDs, leaseRow workerRunLeaseStreamScope, streamName string, direction db.StreamDirection) (db.Session, db.Stream, error) {
	name := strings.TrimSpace(streamName)
	if err := validateSessionStreamName(name); err != nil {
		return db.Session{}, db.Stream{}, badRequest(err)
	}
	session := db.Session{
		ID:            leaseRow.sessionID,
		OrgID:         pgvalue.UUID(leaseIDs.orgID),
		WorkerGroupID: leaseRow.workerGroupID,
		ProjectID:     leaseRow.projectID,
		EnvironmentID: leaseRow.environmentID,
	}
	stream, err := s.ensureSessionStream(ctx, s.db, session, leaseRow.deploymentID, name, direction)
	if err != nil {
		return db.Session{}, db.Stream{}, err
	}
	return session, stream, nil
}

func (s *Server) ensureSessionStream(ctx context.Context, store db.Querier, session db.Session, deploymentID pgtype.UUID, streamName string, direction db.StreamDirection) (db.Stream, error) {
	name := strings.TrimSpace(streamName)
	if err := validateSessionStreamName(name); err != nil {
		return db.Stream{}, badRequest(err)
	}
	stream, err := store.GetSessionStreamByName(ctx, db.GetSessionStreamByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		SessionID:     session.ID,
		Name:          name,
		Direction:     direction,
	})
	if err == nil {
		return stream, nil
	}
	if !isNoRows(err) {
		return db.Stream{}, err
	}
	deploymentStream, err := store.GetDeploymentStreamByName(ctx, db.GetDeploymentStreamByNameParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		DeploymentID:  deploymentID,
		Name:          name,
		Direction:     direction,
	})
	if isNoRows(err) {
		return db.Stream{}, errStreamNotFound
	}
	if err != nil {
		return db.Stream{}, err
	}
	var publicID string
	return createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.Stream, value: &publicID}}, func() (db.Stream, error) {
		return store.EnsureSessionStream(ctx, db.EnsureSessionStreamParams{
			ID:                 pgvalue.UUID(uuid.Must(uuid.NewV7())),
			PublicID:           publicID,
			Metadata:           []byte("{}"),
			DeploymentStreamID: deploymentStream.ID,
			OrgID:              session.OrgID,
			ProjectID:          session.ProjectID,
			EnvironmentID:      session.EnvironmentID,
			SessionID:          session.ID,
		})
	})
}

func (s *Server) readInputStreamRecord(ctx context.Context, store db.Querier, session db.Session, stream db.Stream, afterSequence int64, correlationID string) (db.StreamRecord, bool, error) {
	records, err := store.ListStreamRecords(ctx, db.ListStreamRecordsParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		StreamID:      stream.ID,
		Direction:     db.StreamDirectionInput,
		AfterSequence: afterSequence,
		CorrelationID: pgtype.Text{String: strings.TrimSpace(correlationID), Valid: strings.TrimSpace(correlationID) != ""},
		LimitCount:    1,
	})
	if err != nil {
		return db.StreamRecord{}, false, err
	}
	if len(records) == 0 {
		return db.StreamRecord{}, false, nil
	}
	return records[0], true, nil
}

func (s *Server) publishSessionInputStreamWakeup(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID, sequence int64) {
	if s.eventStream == nil || s.eventStream.redis == nil {
		return
	}
	if err := s.eventStream.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: sessionInputStreamWakeupKey(orgID, streamID),
		MaxLen: activeStreamWakeupMaxLen,
		Approx: true,
		Values: map[string]any{"sequence": sequence},
	}).Err(); err != nil && s.log != nil {
		s.log.Warn("publish session input stream wakeup failed", "stream_id", pgvalue.MustUUIDValue(streamID).String(), "error", err)
	}
}

func (s *Server) latestSessionInputStreamWakeupID(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID) (string, error) {
	if s.eventStream == nil || s.eventStream.redis == nil {
		return "", errActiveStreamUnavailable
	}
	records, err := s.eventStream.redis.XRevRangeN(ctx, sessionInputStreamWakeupKey(orgID, streamID), "+", "-", 1).Result()
	if errors.Is(err, redis.Nil) {
		return "0-0", nil
	}
	if err != nil {
		return "", err
	}
	if len(records) == 0 {
		return "0-0", nil
	}
	return records[0].ID, nil
}

func (s *Server) waitSessionInputStreamWakeup(ctx context.Context, orgID pgtype.UUID, streamID pgtype.UUID, cursor string, maxWait time.Duration) (string, error) {
	if s.eventStream == nil || s.eventStream.redis == nil {
		return cursor, errActiveStreamUnavailable
	}
	blockFor := min(maxWait, activeStreamWakeupBlockEvery)
	if blockFor <= 0 {
		return cursor, nil
	}
	streams, err := s.eventStream.redis.XRead(ctx, &redis.XReadArgs{
		Streams: []string{sessionInputStreamWakeupKey(orgID, streamID), cursor},
		Count:   1,
		Block:   blockFor,
	}).Result()
	if errors.Is(err, redis.Nil) {
		return cursor, nil
	}
	if err != nil {
		return cursor, err
	}
	for _, stream := range streams {
		for _, message := range stream.Messages {
			cursor = message.ID
		}
	}
	return cursor, nil
}

func sessionInputStreamWakeupKey(orgID pgtype.UUID, streamID pgtype.UUID) string {
	return "helmr:session-input-streams:" + pgvalue.MustUUIDValue(orgID).String() + ":" + pgvalue.MustUUIDValue(streamID).String()
}

func workerActiveStreamReadTimeout(timeoutSeconds *int32) time.Duration {
	if timeoutSeconds == nil || *timeoutSeconds <= 0 {
		return activeStreamReadDefaultTimeout
	}
	timeout := time.Duration(*timeoutSeconds) * time.Second
	if timeout > activeStreamReadMaxTimeout {
		return activeStreamReadMaxTimeout
	}
	return timeout
}

func (s *Server) writeWorkerStreamError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errStreamNotFound):
		writeError(w, notFound(err))
	case errors.Is(err, errActiveStreamUnavailable):
		writeError(w, unavailable(err))
	case errors.Is(err, errActiveStreamLeaseLost):
		writeError(w, conflict(err))
	default:
		writeError(w, err)
	}
}
