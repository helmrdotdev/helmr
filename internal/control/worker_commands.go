package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	workerCommandBatchSize     = int32(100)
	workerCommandReplayLimit   = int32(100)
	workerDueCheckpointLimit   = int32(100)
	workerCommandLeaseDuration = 30 * time.Second
	workerCommandIdleEvery     = 100 * time.Millisecond
	workerCommandBlockEvery    = 25 * time.Second
	workerCommandMaxLen        = int64(10000)
)

type WorkerCommandStream struct {
	log   *slog.Logger
	db    db.Querier
	redis redis.Cmdable
}

type workerCommand struct {
	ID                  int64           `json:"id"`
	OrgID               string          `json:"org_id"`
	ProjectID           string          `json:"project_id"`
	EnvironmentID       string          `json:"environment_id"`
	RunID               string          `json:"run_id"`
	RunWaitID           string          `json:"run_wait_id"`
	RunLeaseID          string          `json:"run_lease_id"`
	WorkerInstanceID    string          `json:"worker_instance_id"`
	DeploymentSandboxID string          `json:"deployment_sandbox_id,omitempty"`
	RuntimeInstanceID   string          `json:"runtime_instance_id,omitempty"`
	RuntimeEpoch        int64           `json:"runtime_epoch,omitempty"`
	RunStateVersion     int64           `json:"run_state_version"`
	Kind                string          `json:"kind"`
	Payload             json.RawMessage `json:"payload"`
}

func NewWorkerCommandStream(log *slog.Logger, queries db.Querier, redis redis.Cmdable) (*WorkerCommandStream, error) {
	if queries == nil {
		return nil, errors.New("worker command stream database is required")
	}
	if redis == nil {
		return nil, errors.New("worker command stream redis client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &WorkerCommandStream{log: log, db: queries, redis: redis}, nil
}

func (s *WorkerCommandStream) LatestID(ctx context.Context, workerInstanceID pgtype.UUID) (string, error) {
	records, err := s.redis.XRevRangeN(ctx, workerCommandStreamKey(workerInstanceID), "+", "-", 1).Result()
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

func (s *WorkerCommandStream) Wait(ctx context.Context, workerInstanceID pgtype.UUID, cursor string, maxWait time.Duration) (string, error) {
	blockFor := min(maxWait, workerCommandBlockEvery)
	if blockFor <= 0 {
		return cursor, nil
	}
	streams, err := s.redis.XRead(ctx, &redis.XReadArgs{
		Streams: []string{workerCommandStreamKey(workerInstanceID), cursor},
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

func (s *WorkerCommandStream) RunPublisher(ctx context.Context) error {
	consecutiveFailures := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed, err := s.db.ClaimWorkerCommands(ctx, db.ClaimWorkerCommandsParams{
			RowLimit:      workerCommandBatchSize,
			LeaseDuration: pgvalue.Interval(workerCommandLeaseDuration),
		})
		if err != nil {
			consecutiveFailures++
			s.log.Warn("claim worker commands failed", "error", err)
			if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(consecutiveFailures)); sleepErr != nil {
				return sleepErr
			}
			continue
		}
		consecutiveFailures = 0
		if len(claimed) == 0 {
			if err := sleepWithContext(ctx, workerCommandIdleEvery); err != nil {
				return err
			}
			continue
		}
		for _, row := range claimed {
			if err := s.deliverCommand(ctx, row); err != nil {
				s.log.Warn("deliver worker command failed", "id", row.ID, "error", err)
				if markErr := s.db.MarkWorkerCommandDeliveryFailed(ctx, db.MarkWorkerCommandDeliveryFailedParams{
					ID:                row.ID,
					OrgID:             row.OrgID,
					WorkerInstanceID:  row.WorkerInstanceID,
					LastDeliveryError: err.Error(),
					RetryAfter:        pgvalue.Interval(eventPublisherBackoff(int(row.DeliveryAttempts))),
				}); markErr != nil {
					s.log.Warn("mark worker command delivery failed", "id", row.ID, "error", markErr)
					if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.DeliveryAttempts))); sleepErr != nil {
						return sleepErr
					}
				}
				continue
			}
			if _, err := s.db.MarkWorkerCommandDelivered(ctx, db.MarkWorkerCommandDeliveredParams{
				ID:               row.ID,
				OrgID:            row.OrgID,
				WorkerInstanceID: row.WorkerInstanceID,
			}); err != nil {
				s.log.Warn("mark worker command delivered failed", "id", row.ID, "error", err)
				if sleepErr := sleepWithContext(ctx, eventPublisherBackoff(int(row.DeliveryAttempts))); sleepErr != nil {
					return sleepErr
				}
			}
		}
	}
}

func (s *WorkerCommandStream) deliverCommand(ctx context.Context, row db.ClaimWorkerCommandsRow) error {
	payload, err := json.Marshal(workerCommand{
		ID:                  row.ID,
		OrgID:               pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:           pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		RunID:               pgvalue.UUIDString(row.RunID),
		RunWaitID:           pgvalue.UUIDString(row.RunWaitID),
		RunLeaseID:          pgvalue.UUIDString(row.RunLeaseID),
		WorkerInstanceID:    pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
		DeploymentSandboxID: pgvalue.UUIDString(row.DeploymentSandboxID),
		RuntimeInstanceID:   pgvalue.UUIDString(row.RuntimeInstanceID),
		RuntimeEpoch:        workerCommandRuntimeEpoch(row.RuntimeEpoch),
		RunStateVersion:     workerCommandRunStateVersion(row.RunStateVersion),
		Kind:                string(row.Kind),
		Payload:             json.RawMessage(row.Payload),
	})
	if err != nil {
		return fmt.Errorf("encode worker command: %w", err)
	}
	err = s.redis.XAdd(ctx, &redis.XAddArgs{
		Stream: workerCommandStreamKey(row.WorkerInstanceID),
		MaxLen: workerCommandMaxLen,
		Approx: true,
		ID:     redisEventID(row.ID),
		Values: map[string]any{"command": string(payload)},
	}).Err()
	if err == nil || redisIDAlreadyExists(err) {
		return nil
	}
	return err
}

func (s *Server) workerReadCommands(w http.ResponseWriter, r *http.Request) {
	if s.workerCommandStream == nil {
		writeError(w, unavailable(errors.New("worker command stream is unavailable")))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, unavailable(errors.New("worker command stream requires streaming responses")))
		return
	}
	afterID, err := parseWorkerCommandAfterID(r)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	worker := workerFromContext(r.Context())
	workerInstanceID := pgvalue.UUID(worker.WorkerInstanceID)
	cursor, err := s.workerCommandStream.LatestID(r.Context(), workerInstanceID)
	if err != nil {
		writeError(w, unavailable(errors.New("worker command stream is unavailable")))
		return
	}

	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		if err := r.Context().Err(); err != nil {
			return
		}
		if _, err := s.db.CreateDueLiveRuntimeCheckpointWaitCommandsForWorker(r.Context(), db.CreateDueLiveRuntimeCheckpointWaitCommandsForWorkerParams{
			WorkerInstanceID: workerInstanceID,
			LimitCount:       workerDueCheckpointLimit,
		}); err != nil {
			s.log.Warn("create due live runtime checkpoint commands failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeWorkerCommandSSEError(w, flusher, err)
			return
		}
		rows, err := s.db.ListWorkerCommandsAfter(r.Context(), db.ListWorkerCommandsAfterParams{
			WorkerInstanceID: workerInstanceID,
			AfterID:          afterID,
			LimitCount:       workerCommandReplayLimit,
		})
		if err != nil {
			s.log.Warn("list worker commands failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeWorkerCommandSSEError(w, flusher, err)
			return
		}
		for _, row := range rows {
			if err := writeWorkerCommandSSE(w, flusher, row); err != nil {
				return
			}
			afterID = row.ID
		}
		cursor, err = advanceWorkerCommandRedisCursor(cursor, afterID)
		if err != nil {
			s.log.Warn("advance worker command cursor failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeWorkerCommandSSEError(w, flusher, err)
			return
		}
		if len(rows) == int(workerCommandReplayLimit) {
			continue
		}
		nextCursor, err := s.workerCommandStream.Wait(r.Context(), workerInstanceID, cursor, workerCommandBlockEvery)
		if err != nil {
			s.log.Warn("wait worker command failed", "worker_instance_id", worker.WorkerInstanceID.String(), "error", err)
			writeWorkerCommandSSEError(w, flusher, err)
			return
		}
		if nextCursor == cursor {
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			continue
		}
		cursor = nextCursor
	}
}

func (s *Server) workerAcknowledgeCommand(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCommandAckRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker command ack request JSON: %w", err)))
		return
	}
	if request.ID <= 0 {
		writeError(w, badRequest(errors.New("id must be positive")))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.AcknowledgeWorkerCommand(r.Context(), db.AcknowledgeWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		ID:               request.ID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker command not found")))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCommandAckResponse{
		ID:               row.ID,
		WorkerInstanceID: pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
	})
}

func (s *Server) workerAcceptCommand(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCommandAcceptRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker command accept request JSON: %w", err)))
		return
	}
	if request.ID <= 0 {
		writeError(w, badRequest(errors.New("id must be positive")))
		return
	}
	worker := workerFromContext(r.Context())
	row, err := s.db.AcceptWorkerCommand(r.Context(), db.AcceptWorkerCommandParams{
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		ID:               request.ID,
	})
	if isNoRows(err) {
		writeError(w, notFound(errors.New("worker command not found")))
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCommandAcceptResponse{
		ID:               row.ID,
		WorkerInstanceID: pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
	})
}

func parseWorkerCommandAfterID(r *http.Request) (int64, error) {
	value := r.URL.Query().Get("after_id")
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, errors.New("after_id must be non-negative")
	}
	return id, nil
}

func writeWorkerCommandSSE(w io.Writer, flusher http.Flusher, row db.WorkerCommand) error {
	payload, err := json.Marshal(workerCommandResponse(row))
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\nevent: worker_command\ndata: %s\n\n", row.ID, payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeWorkerCommandSSEError(w io.Writer, flusher http.Flusher, err error) {
	payload, marshalErr := json.Marshal(map[string]string{"error": err.Error()})
	if marshalErr != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	flusher.Flush()
}

func workerCommandResponse(row db.WorkerCommand) api.WorkerCommand {
	payload := json.RawMessage(row.Payload)
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	return api.WorkerCommand{
		ID:                  row.ID,
		OrgID:               pgvalue.MustUUIDValue(row.OrgID).String(),
		ProjectID:           pgvalue.MustUUIDValue(row.ProjectID).String(),
		EnvironmentID:       pgvalue.MustUUIDValue(row.EnvironmentID).String(),
		RunID:               pgvalue.UUIDString(row.RunID),
		RunWaitID:           pgvalue.UUIDString(row.RunWaitID),
		RunLeaseID:          pgvalue.UUIDString(row.RunLeaseID),
		WorkerInstanceID:    pgvalue.MustUUIDValue(row.WorkerInstanceID).String(),
		DeploymentSandboxID: pgvalue.UUIDString(row.DeploymentSandboxID),
		RuntimeInstanceID:   pgvalue.UUIDString(row.RuntimeInstanceID),
		RuntimeEpoch:        workerCommandRuntimeEpoch(row.RuntimeEpoch),
		RunStateVersion:     workerCommandRunStateVersion(row.RunStateVersion),
		Kind:                string(row.Kind),
		Payload:             payload,
	}
}

func workerCommandRuntimeEpoch(value pgtype.Int8) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func workerCommandRunStateVersion(value pgtype.Int8) int64 {
	if !value.Valid {
		return 0
	}
	return value.Int64
}

func workerCommandStreamKey(workerInstanceID pgtype.UUID) string {
	return "helmr:worker-commands:" + pgvalue.MustUUIDValue(workerInstanceID).String()
}

func advanceWorkerCommandRedisCursor(cursor string, afterID int64) (string, error) {
	if afterID <= 0 {
		return cursor, nil
	}
	cursorSeq, err := redisSeq(cursor)
	if err != nil {
		return "", err
	}
	if afterID <= cursorSeq {
		return cursor, nil
	}
	return redisEventID(afterID), nil
}
