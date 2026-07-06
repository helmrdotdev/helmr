package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

type workerStreamWaitParams struct {
	Stream        string `json:"stream"`
	CorrelationID string `json:"correlation_id,omitempty"`
	AfterSequence int64  `json:"after_sequence,omitempty"`
}

func (s *Server) createWorkerStreamWait(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runWait db.RunWait, request api.WorkerCreateRunWaitRequest) error {
	params, stream, err := s.workerInputStreamWaitTarget(ctx, store, scope, request)
	if err != nil {
		return err
	}
	if _, err := store.CreateStreamWait(ctx, db.CreateStreamWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunWaitID:     runWait.ID,
		StreamID:      stream.ID,
		AfterSequence: params.AfterSequence,
		CorrelationID: strings.TrimSpace(params.CorrelationID),
	}); err != nil {
		return err
	}
	return nil
}

func (s *Server) matchBufferedWorkerStreamWait(ctx context.Context, scope db.GetWorkerRunWaitScopeRow, request api.WorkerCreateRunWaitRequest) (json.RawMessage, bool, error) {
	params, stream, err := s.workerInputStreamWaitTarget(ctx, s.db, scope, request)
	if err != nil {
		return nil, false, err
	}
	session := db.Session{
		ID:            scope.SessionID,
		OrgID:         scope.OrgID,
		WorkerGroupID: scope.WorkerGroupID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
	}
	record, found, err := s.readInputStreamRecord(ctx, s.db, session, stream, params.AfterSequence, params.CorrelationID)
	if err != nil {
		return nil, false, err
	}
	if !found {
		return nil, false, nil
	}
	if err := s.sessionRunRequestWorkflow().consumeByActiveRun(ctx, session, scope.RunID, record.ID); err != nil {
		return nil, false, err
	}
	payload, err := json.Marshal(map[string]any{
		"stream":   stream.Name,
		"sequence": record.Sequence,
		"data":     json.RawMessage(record.Data),
	})
	if err != nil {
		return nil, false, err
	}
	return payload, true, nil
}

func (s *Server) workerInputStreamWaitTarget(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, request api.WorkerCreateRunWaitRequest) (workerStreamWaitParams, db.Stream, error) {
	var params workerStreamWaitParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return workerStreamWaitParams{}, db.Stream{}, badRequest(fmt.Errorf("invalid stream wait params: %w", err))
	}
	if params.AfterSequence < 0 {
		return workerStreamWaitParams{}, db.Stream{}, badRequest(errors.New("stream wait params.after_sequence must be non-negative"))
	}
	streamName := strings.TrimSpace(params.Stream)
	if streamName == "" {
		return workerStreamWaitParams{}, db.Stream{}, badRequest(errors.New("stream wait params.stream is required"))
	}
	stream, err := s.ensureSessionStream(ctx, store, db.Session{
		ID:                 scope.SessionID,
		OrgID:              scope.OrgID,
		WorkerGroupID:      scope.WorkerGroupID,
		ProjectID:          scope.ProjectID,
		EnvironmentID:      scope.EnvironmentID,
		ActiveDeploymentID: scope.DeploymentID,
		TaskID:             scope.TaskID,
	}, scope.DeploymentID, streamName, db.StreamDirectionInput)
	if err != nil {
		return workerStreamWaitParams{}, db.Stream{}, err
	}
	return params, stream, nil
}
