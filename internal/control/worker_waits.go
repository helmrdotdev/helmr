package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workspace"
	"github.com/jackc/pgx/v5/pgtype"
)

type workerStreamWaitParams struct {
	Stream        string `json:"stream"`
	CorrelationID string `json:"correlation_id,omitempty"`
	AfterSequence int64  `json:"after_sequence,omitempty"`
}

type workerTokenWaitParams struct {
	TokenID string `json:"token_id"`
}

const (
	defaultLiveWaitCheckpointDelay = 5 * time.Second
	shortTimerLiveMaxDuration      = 5 * time.Second
	shortTimerCheckpointGrace      = 1 * time.Second
	interactiveLiveWaitDelay       = 2 * time.Minute
)

type workerRunWaitPolicyReason string

const (
	workerRunWaitPolicyInteractiveHotWindow    workerRunWaitPolicyReason = "interactive_wait_hot_window"
	workerRunWaitPolicyInteractiveIdleTimeout  workerRunWaitPolicyReason = "interactive_wait_idle_timeout"
	workerRunWaitPolicyInteractiveUntilTimeout workerRunWaitPolicyReason = "interactive_wait_timeout_within_hot_window"
	workerRunWaitPolicyShortTimerLiveUntilFire workerRunWaitPolicyReason = "short_timer_live_until_fire"
	workerRunWaitPolicyLongTimerPark           workerRunWaitPolicyReason = "long_timer_checkpoint_delay"
	workerRunWaitPolicyDefaultCheckpointDelay  workerRunWaitPolicyReason = "default_checkpoint_delay"
)

type workerRunWaitPolicy struct {
	CheckpointDelay time.Duration
	Reason          workerRunWaitPolicyReason
}

func (s *Server) workerCreateRunWait(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCreateRunWaitRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker wait request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	scope, err := s.db.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(orgID),
		RunID:            pgvalue.UUID(runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker run wait scope"))
		return
	}
	response, err := s.createWorkerRunWait(r.Context(), scope, request)
	if err != nil {
		s.writeWorkerWaitError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createWorkerRunWait(ctx context.Context, scope db.GetWorkerRunWaitScopeRow, request api.WorkerCreateRunWaitRequest) (api.WorkerCreateRunWaitResponse, error) {
	switch request.Kind {
	case api.WorkerRunWaitKindStream, api.WorkerRunWaitKindToken, api.WorkerRunWaitKindTimer:
	default:
		return api.WorkerCreateRunWaitResponse{}, badRequest(fmt.Errorf("unsupported wait kind %q", request.Kind))
	}
	if request.Kind == api.WorkerRunWaitKindStream {
		resolution, matched, err := s.matchBufferedWorkerStreamWait(ctx, scope, request)
		if err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		if matched {
			return api.WorkerCreateRunWaitResponse{
				RunID:          pgvalue.MustUUIDValue(scope.RunID).String(),
				ResolutionKind: "completed",
				Resolution:     resolution,
			}, nil
		}
	}
	if request.Kind == api.WorkerRunWaitKindToken {
		resolutionKind, resolution, matched, err := s.matchImmediateWorkerTokenWait(ctx, scope, request)
		if err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		if matched {
			return api.WorkerCreateRunWaitResponse{
				RunID:          pgvalue.MustUUIDValue(scope.RunID).String(),
				ResolutionKind: resolutionKind,
				Resolution:     resolution,
			}, nil
		}
	}
	if request.Kind == api.WorkerRunWaitKindTimer {
		if request.TimeoutSeconds == nil || *request.TimeoutSeconds <= 0 {
			return api.WorkerCreateRunWaitResponse{}, badRequest(errors.New("timer wait requires timeout_seconds"))
		}
	}
	runWaitID := uuid.Must(uuid.NewV7())
	var timeoutAt pgtype.Timestamptz
	if request.Kind != api.WorkerRunWaitKindTimer {
		timeoutAt = workerWaitTimeoutAt(request.TimeoutSeconds)
	}
	waitPolicy := selectWorkerRunWaitPolicy(request)
	var response api.WorkerCreateRunWaitResponse
	err := s.inTx(ctx, func(work *txWork) error {
		createdRunWait, err := work.q.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
			ID:               pgvalue.UUID(runWaitID),
			OrgID:            scope.OrgID,
			ProjectID:        scope.ProjectID,
			EnvironmentID:    scope.EnvironmentID,
			RunID:            scope.RunID,
			RunLeaseID:       scope.CurrentRunLeaseID,
			WorkerInstanceID: scope.WorkerInstanceID,
			Kind:             db.RunWaitKind(request.Kind),
			CorrelationID:    strings.TrimSpace(request.CorrelationID),
			TimeoutAt:        timeoutAt,
			CheckpointDelay:  pgvalue.Interval(waitPolicy.CheckpointDelay),
		})
		if err != nil {
			return err
		}
		runWait := runWaitFromCreateHotRunWait(createdRunWait)
		if s.log != nil && s.log.Enabled(ctx, slog.LevelDebug) {
			s.log.Debug("worker run wait policy selected",
				"org_id", pgvalue.UUIDString(scope.OrgID),
				"project_id", pgvalue.UUIDString(scope.ProjectID),
				"environment_id", pgvalue.UUIDString(scope.EnvironmentID),
				"run_id", pgvalue.UUIDString(scope.RunID),
				"run_wait_id", pgvalue.UUIDString(runWait.ID),
				"kind", request.Kind,
				"timeout_seconds", optionalInt32Value(request.TimeoutSeconds),
				"idle_timeout_seconds", optionalInt32Value(request.IdleTimeoutSeconds),
				"checkpoint_delay_ms", waitPolicy.CheckpointDelay.Milliseconds(),
				"reason", waitPolicy.Reason,
			)
		}
		response = api.WorkerCreateRunWaitResponse{
			RunID:             pgvalue.MustUUIDValue(scope.RunID).String(),
			RunWaitID:         pgvalue.MustUUIDValue(runWait.ID).String(),
			RuntimeInstanceID: pgvalue.UUIDString(runWait.OwnerRuntimeInstanceID),
			RuntimeEpoch:      pgvalue.Int8Value(runWait.OwnerRuntimeEpoch),
			CheckpointDelayMs: waitPolicy.CheckpointDelay.Milliseconds(),
		}
		if scope.DirtyGeneration == 0 {
			runWait, err = work.q.SetRunWaitWorkspaceVersion(ctx, db.SetRunWaitWorkspaceVersionParams{
				OrgID:              scope.OrgID,
				ProjectID:          scope.ProjectID,
				EnvironmentID:      scope.EnvironmentID,
				ID:                 runWait.ID,
				RunID:              scope.RunID,
				WorkspaceVersionID: scope.WorkspaceCurrentVersionID,
			})
			if err != nil {
				return errors.New("record clean run wait workspace version")
			}
			response.WorkspaceVersionID = pgvalue.MustUUIDValue(runWait.WorkspaceVersionID).String()
		}
		switch request.Kind {
		case api.WorkerRunWaitKindStream:
			if err := s.createWorkerStreamWait(ctx, work.q, scope, runWait, request); err != nil {
				return err
			}
		case api.WorkerRunWaitKindToken:
			resolutionKind, resolution, matched, err := s.createWorkerTokenWait(ctx, work.q, scope, runWait, request)
			if err != nil {
				return err
			}
			if matched {
				response = api.WorkerCreateRunWaitResponse{
					RunID:          pgvalue.MustUUIDValue(scope.RunID).String(),
					ResolutionKind: resolutionKind,
					Resolution:     resolution,
				}
			}
		case api.WorkerRunWaitKindTimer:
			if err := s.createWorkerTimerWait(ctx, work.q, scope, runWait, request); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.WorkerCreateRunWaitResponse{}, err
	}
	return response, nil
}

func runWaitFromCreateHotRunWait(row db.CreateHotRunWaitRow) db.RunWait {
	return db.RunWait(row)
}

func selectWorkerRunWaitPolicy(request api.WorkerCreateRunWaitRequest) workerRunWaitPolicy {
	switch request.Kind {
	case api.WorkerRunWaitKindToken, api.WorkerRunWaitKindStream:
		liveDelay := interactiveLiveWaitDelay
		reason := workerRunWaitPolicyInteractiveHotWindow
		if request.IdleTimeoutSeconds != nil && *request.IdleTimeoutSeconds > 0 {
			idleTimeoutDuration := time.Duration(*request.IdleTimeoutSeconds) * time.Second
			if idleTimeoutDuration < liveDelay {
				liveDelay = idleTimeoutDuration
				reason = workerRunWaitPolicyInteractiveIdleTimeout
			}
		}
		if request.TimeoutSeconds != nil && *request.TimeoutSeconds > 0 {
			timeoutDuration := time.Duration(*request.TimeoutSeconds) * time.Second
			if timeoutDuration <= liveDelay {
				return workerRunWaitPolicy{
					CheckpointDelay: timeoutDuration + shortTimerCheckpointGrace,
					Reason:          workerRunWaitPolicyInteractiveUntilTimeout,
				}
			}
		}
		return workerRunWaitPolicy{
			CheckpointDelay: liveDelay,
			Reason:          reason,
		}
	case api.WorkerRunWaitKindTimer:
		if request.TimeoutSeconds != nil && *request.TimeoutSeconds > 0 {
			timerDuration := time.Duration(*request.TimeoutSeconds) * time.Second
			if timerDuration <= shortTimerLiveMaxDuration {
				return workerRunWaitPolicy{
					CheckpointDelay: timerDuration + shortTimerCheckpointGrace,
					Reason:          workerRunWaitPolicyShortTimerLiveUntilFire,
				}
			}
			return workerRunWaitPolicy{
				CheckpointDelay: defaultLiveWaitCheckpointDelay,
				Reason:          workerRunWaitPolicyLongTimerPark,
			}
		}
	}
	return workerRunWaitPolicy{
		CheckpointDelay: defaultLiveWaitCheckpointDelay,
		Reason:          workerRunWaitPolicyDefaultCheckpointDelay,
	}
}

func optionalInt32Value(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
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

func (s *Server) matchImmediateWorkerTokenWait(ctx context.Context, scope db.GetWorkerRunWaitScopeRow, request api.WorkerCreateRunWaitRequest) (string, json.RawMessage, bool, error) {
	tokenID, err := workerTokenWaitTokenID(request)
	if err != nil {
		return "", nil, false, err
	}
	token, err := s.db.GetToken(ctx, db.GetTokenParams{
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            pgvalue.UUID(tokenID),
	})
	if isNoRows(err) {
		return "", nil, false, errTokenNotFound
	}
	if err != nil {
		return "", nil, false, err
	}
	return workerTokenResolution(token)
}

func (s *Server) createWorkerTokenWait(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runWait db.RunWait, request api.WorkerCreateRunWaitRequest) (string, json.RawMessage, bool, error) {
	tokenID, err := workerTokenWaitTokenID(request)
	if err != nil {
		return "", nil, false, err
	}
	tokenWait, err := store.CreateTokenWait(ctx, db.CreateTokenWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunWaitID:     runWait.ID,
		TokenID:       pgvalue.UUID(tokenID),
	})
	if err != nil {
		return "", nil, false, err
	}
	if _, err := store.ResolveImmediateTokenWait(ctx, db.ResolveImmediateTokenWaitParams{OrgID: scope.OrgID, ID: tokenWait.ID}); isNoRows(err) {
		return "", nil, false, nil
	} else if err != nil {
		return "", nil, false, err
	}
	token, err := store.GetToken(ctx, db.GetTokenParams{
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            pgvalue.UUID(tokenID),
	})
	if err != nil {
		return "", nil, false, err
	}
	return workerTokenResolution(token)
}

func workerTokenWaitTokenID(request api.WorkerCreateRunWaitRequest) (uuid.UUID, error) {
	var params workerTokenWaitParams
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return uuid.UUID{}, badRequest(fmt.Errorf("invalid token wait params: %w", err))
	}
	tokenID, err := uuid.Parse(strings.TrimSpace(params.TokenID))
	if err != nil {
		return uuid.UUID{}, badRequest(errors.New("token wait params.token_id must be a UUID"))
	}
	return tokenID, nil
}

func workerTokenResolution(token db.Token) (string, json.RawMessage, bool, error) {
	switch {
	case token.State == db.TokenStateCancelled:
		return "cancelled", json.RawMessage(`null`), true, nil
	case token.State == db.TokenStateExpired:
		return "timed_out", json.RawMessage(`null`), true, nil
	case token.State == db.TokenStatePending && pgvalue.Time(token.TimeoutAt).Before(time.Now()):
		return "timed_out", json.RawMessage(`null`), true, nil
	case token.State != db.TokenStateCompleted:
		return "", nil, false, nil
	case len(token.CompletionData) == 0:
		return "completed", json.RawMessage(`null`), true, nil
	default:
		return "completed", json.RawMessage(token.CompletionData), true, nil
	}
}

func (s *Server) createWorkerTimerWait(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runWait db.RunWait, request api.WorkerCreateRunWaitRequest) error {
	if request.TimeoutSeconds == nil || *request.TimeoutSeconds <= 0 {
		return badRequest(errors.New("timer wait requires timeout_seconds"))
	}
	_, err := store.CreateTimerWait(ctx, db.CreateTimerWaitParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunWaitID:     runWait.ID,
		FireAt:        pgvalue.Timestamptz(time.Now().Add(time.Duration(*request.TimeoutSeconds) * time.Second)),
	})
	return err
}

func (s *Server) workerClaimRuntimeCheckpointWait(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointClaimRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker checkpoint claim request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	scope, err := s.db.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(orgID),
		RunID:            pgvalue.UUID(runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		stale, staleErr := s.writeStaleCheckpointCommandIfAdvanced(r.Context(), w, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID)
		if staleErr != nil {
			writeError(w, errors.New("load stale run wait checkpoint claim"))
			return
		}
		if stale {
			return
		}
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker run wait scope"))
		return
	}
	claim, err := s.db.ClaimRuntimeCheckpointWait(r.Context(), db.ClaimRuntimeCheckpointWaitParams{
		RuntimeCheckpointID: pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:               scope.OrgID,
		ProjectID:           scope.ProjectID,
		EnvironmentID:       scope.EnvironmentID,
		RunID:               pgvalue.UUID(runID),
		RunWaitID:           pgvalue.UUID(runWaitID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		stale, staleErr := s.writeStaleCheckpointCommandIfAdvanced(r.Context(), w, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID)
		if staleErr != nil {
			writeError(w, errors.New("load stale run wait checkpoint claim"))
			return
		}
		if stale {
			return
		}
		writeError(w, conflict(errors.New("run wait checkpoint claim lost")))
		return
	}
	if err != nil {
		writeError(w, errors.New("claim run wait checkpoint"))
		return
	}
	response := api.WorkerCheckpointClaimResponse{
		RunID:            runID.String(),
		RunWaitID:        runWaitID.String(),
		Status:           "claimed",
		CheckpointID:     pgvalue.MustUUIDValue(claim.RuntimeCheckpointID).String(),
		CaptureWorkspace: claim.DirtyGeneration > 0,
	}
	if claim.WorkspaceVersionID.Valid {
		response.WorkspaceVersionID = pgvalue.MustUUIDValue(claim.WorkspaceVersionID).String()
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeStaleCheckpointCommandIfAdvanced(ctx context.Context, w http.ResponseWriter, orgID uuid.UUID, runID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID, runWaitID uuid.UUID) (bool, error) {
	runWait, err := s.db.GetRunWaitByRun(ctx, db.GetRunWaitByRunParams{
		OrgID: pgvalue.UUID(orgID),
		RunID: pgvalue.UUID(runID),
		ID:    pgvalue.UUID(runWaitID),
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if runWait.OwnerRunLeaseID != pgvalue.UUID(runLeaseID) ||
		runWait.OwnerWorkerInstanceID != pgvalue.UUID(workerInstanceID) ||
		!isStaleCheckpointCommandState(runWait.State) {
		return false, nil
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointClaimResponse{
		RunID:     runID.String(),
		RunWaitID: runWaitID.String(),
		Status:    "stale",
	})
	return true, nil
}

func isStaleCheckpointCommandState(state db.RunWaitState) bool {
	switch state {
	case db.RunWaitStateCheckpointedWaiting,
		db.RunWaitStateResolvedLive,
		db.RunWaitStateResolvedCheckpointed,
		db.RunWaitStateExpired,
		db.RunWaitStateResuming,
		db.RunWaitStateResumed,
		db.RunWaitStateCancelled,
		db.RunWaitStateFailed:
		return true
	default:
		return false
	}
}

func (s *Server) workerCaptureRunWaitWorkspace(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerRunWaitWorkspaceCaptureRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker run wait workspace capture request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	if err := validateWorkerWorkspaceCapture(request.WorkspaceCapture); err != nil {
		writeError(w, badRequest(err))
		return
	}
	var response api.WorkerRunWaitWorkspaceCaptureResponse
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		runWait, err := work.q.GetRunWait(r.Context(), db.GetRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			ID:            pgvalue.UUID(runWaitID),
		})
		if isNoRows(err) {
			return notFound(errors.New("run wait not found"))
		}
		if err != nil {
			return errors.New("load run wait")
		}
		if pgvalue.MustUUIDValue(runWait.RunID) != runID || runWait.State != db.RunWaitStateCheckpointing {
			return conflict(errors.New("run wait is not checkpointing for this run"))
		}
		if runWait.WorkspaceVersionID.Valid {
			response = api.WorkerRunWaitWorkspaceCaptureResponse{
				RunID:              runID.String(),
				RunWaitID:          strings.TrimSpace(request.RunWaitID),
				CheckpointID:       strings.TrimSpace(request.CheckpointID),
				WorkspaceVersionID: pgvalue.MustUUIDValue(runWait.WorkspaceVersionID).String(),
			}
			return nil
		}
		capture := request.WorkspaceCapture
		if _, err := work.q.UpsertCasObject(r.Context(), db.UpsertCasObjectParams{
			Digest:    strings.TrimSpace(capture.Digest),
			SizeBytes: capture.SizeBytes,
			MediaType: strings.TrimSpace(capture.MediaType),
		}); err != nil {
			return errors.New("record run wait workspace capture CAS object")
		}
		artifact, err := work.q.CreateArtifact(r.Context(), db.CreateArtifactParams{
			ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
			OrgID:                     scope.OrgID,
			ProjectID:                 scope.ProjectID,
			EnvironmentID:             scope.EnvironmentID,
			Digest:                    strings.TrimSpace(capture.Digest),
			Kind:                      db.ArtifactKindWorkspaceVersion,
			SizeBytes:                 capture.SizeBytes,
			MediaType:                 strings.TrimSpace(capture.MediaType),
			CreatedByWorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if err != nil {
			return errors.New("record run wait workspace capture artifact")
		}
		version, err := work.q.PromoteWorkspaceCapture(r.Context(), db.PromoteWorkspaceCaptureParams{
			OrgID:              scope.OrgID,
			WriteLeaseID:       scope.WorkspaceLeaseID,
			FencingToken:       scope.WorkspaceFencingToken,
			DirtyGeneration:    scope.DirtyGeneration,
			ArtifactID:         artifact.ID,
			SizeBytes:          capture.SizeBytes,
			ArtifactEncoding:   strings.TrimSpace(capture.Encoding),
			ContentDigest:      strings.TrimSpace(capture.Digest),
			VersionID:          pgvalue.UUID(uuid.Must(uuid.NewV7())),
			Kind:               db.WorkspaceVersionKindSystem,
			ArtifactEntryCount: capture.EntryCount,
			Message:            "system capture before parked wait",
		})
		if isNoRows(err) {
			return conflict(codedError{code: "workspace_capture_rejected", message: "workspace capture is stale"})
		}
		if err != nil {
			return errors.New("promote run wait workspace capture")
		}
		if _, err := work.q.SetRunWaitWorkspaceVersion(r.Context(), db.SetRunWaitWorkspaceVersionParams{
			OrgID:              scope.OrgID,
			ProjectID:          scope.ProjectID,
			EnvironmentID:      scope.EnvironmentID,
			ID:                 pgvalue.UUID(runWaitID),
			RunID:              scope.RunID,
			WorkspaceVersionID: version.ID,
		}); err != nil {
			return errors.New("record run wait workspace version")
		}
		response = api.WorkerRunWaitWorkspaceCaptureResponse{
			RunID:              runID.String(),
			RunWaitID:          strings.TrimSpace(request.RunWaitID),
			CheckpointID:       strings.TrimSpace(request.CheckpointID),
			WorkspaceVersionID: pgvalue.MustUUIDValue(version.ID).String(),
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func validateWorkerWorkspaceCapture(capture api.WorkerWorkspaceArtifact) error {
	if strings.TrimSpace(capture.Digest) == "" {
		return errors.New("workspace_capture.digest is required")
	}
	if strings.TrimSpace(capture.MediaType) != workspace.ArtifactMediaType {
		return errors.New("workspace_capture.media_type is unsupported")
	}
	if strings.TrimSpace(capture.Encoding) != workspace.ArtifactEncoding {
		return errors.New("workspace_capture.encoding is unsupported")
	}
	if capture.SizeBytes <= 0 {
		return errors.New("workspace_capture.size_bytes must be positive")
	}
	if capture.SizeBytes > workspace.MaxArtifactArchiveBytes {
		return fmt.Errorf("workspace_capture.size_bytes exceeds max %d", workspace.MaxArtifactArchiveBytes)
	}
	if capture.EntryCount < 0 {
		return errors.New("workspace_capture.entry_count must be non-negative")
	}
	if capture.EntryCount > workspace.MaxArtifactEntries {
		return fmt.Errorf("workspace_capture.entry_count exceeds max %d", workspace.MaxArtifactEntries)
	}
	return nil
}

func (s *Server) workerMarkCheckpointReady(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointReadyRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime checkpoint ready request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.WorkerCommandID <= 0 {
		writeError(w, badRequest(errors.New("worker_command_id must be positive")))
		return
	}
	if err := validateWorkerCheckpointManifest(request.Manifest); err != nil {
		writeError(w, badRequest(err))
		return
	}
	runtimeSubstrateArtifactID, err := checkpointRuntimeSubstrateArtifactID(request.Manifest)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	errReadyCheckpointReplay := errors.New("ready runtime checkpoint replay")
	replayConflict := conflict(errors.New("runtime checkpoint cannot be marked ready for this wait"))
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			replayConflict = conflict(errors.New("worker run lease is not active"))
			return errReadyCheckpointReplay
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		artifactIDs, err := s.createRuntimeCheckpointArtifacts(r.Context(), work.q, pgvalue.UUID(worker.WorkerInstanceID), scope, request.Manifest)
		if err != nil {
			return errors.New("create runtime checkpoint artifacts")
		}
		manifest, err := json.Marshal(request.Manifest)
		if err != nil {
			return errors.New("encode runtime checkpoint manifest")
		}
		created, err := work.q.CreateReadyRuntimeCheckpointForRunWait(r.Context(), db.CreateReadyRuntimeCheckpointForRunWaitParams{
			WorkerCommandID:            request.WorkerCommandID,
			OrgID:                      scope.OrgID,
			ProjectID:                  scope.ProjectID,
			EnvironmentID:              scope.EnvironmentID,
			RunWaitID:                  pgvalue.UUID(runWaitID),
			RunID:                      pgvalue.UUID(runID),
			RunLeaseID:                 pgvalue.UUID(runLeaseID),
			WorkerInstanceID:           pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeCheckpointID:        pgvalue.UUID(runtimeCheckpointID),
			RuntimeBackend:             request.Manifest.RecoveryPoint.Runtime.Backend,
			RuntimeID:                  request.Manifest.RecoveryPoint.Runtime.ID,
			RuntimeArch:                request.Manifest.RecoveryPoint.Runtime.Arch,
			RuntimeABI:                 request.Manifest.RecoveryPoint.Runtime.ABI,
			KernelDigest:               request.Manifest.RecoveryPoint.Runtime.KernelDigest,
			InitramfsDigest:            request.Manifest.RecoveryPoint.Runtime.InitramfsDigest,
			RootfsDigest:               request.Manifest.RecoveryPoint.Runtime.RootfsDigest,
			RuntimeConfigDigest:        request.Manifest.RecoveryPoint.Runtime.ConfigDigest,
			RuntimeSubstrateArtifactID: runtimeSubstrateArtifactID,
			CniProfile:                 scope.WorkerCniProfile,
			SubstrateDigest:            checkpointSubstrateDigest(request.Manifest),
			Manifest:                   manifest,
		})
		if isNoRows(err) {
			return errReadyCheckpointReplay
		}
		if err != nil {
			s.log.Error("mark runtime checkpoint ready failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"run_lease_id", runLeaseID,
				"runtime_checkpoint_id", runtimeCheckpointID,
			)
			return errors.New("mark runtime checkpoint ready")
		}
		if err := s.createRuntimeCheckpointArtifactRows(r.Context(), work.q, scope, created.ID, request.Manifest, artifactIDs); err != nil {
			s.log.Error("create runtime checkpoint artifact rows failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
				"runtime_checkpoint_id", runtimeCheckpointID,
			)
			return errors.New("create runtime checkpoint artifact rows")
		}
		if err := s.resolveReadyRunWait(r.Context(), work.q, scope, pgvalue.UUID(runWaitID)); err != nil {
			s.log.Error("resolve ready run wait failed",
				"error", err,
				"worker_instance_id", worker.WorkerInstanceID,
				"run_id", runID,
				"run_wait_id", runWaitID,
			)
			return errors.New("resolve ready run wait")
		}
		if err := acknowledgeCheckpointWorkerCommand(r.Context(), work.q, scope, request.WorkerCommandID, runID, runWaitID, runLeaseID, worker.WorkerInstanceID); err != nil {
			return err
		}
		work.AfterCommit(func(ctx context.Context) error {
			s.requeueResolvedRunWaits(ctx, scope.OrgID)
			return nil
		})
		return nil
	})
	if errors.Is(err, errReadyCheckpointReplay) {
		replayed, replayErr := s.writeAcknowledgedReadyRuntimeCheckpointReplay(r.Context(), w, s.db, orgID, runID, runLeaseID, worker.WorkerInstanceID, runWaitID, runtimeCheckpointID, request.WorkerCommandID)
		if replayErr != nil {
			writeError(w, errors.New("load acknowledged ready runtime checkpoint replay"))
			return
		}
		if replayed {
			return
		}
		writeError(w, replayConflict)
		return
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: runtimeCheckpointID.String(),
	})
}

func (s *Server) writeAcknowledgedReadyRuntimeCheckpointReplay(ctx context.Context, w http.ResponseWriter, store db.Querier, orgID uuid.UUID, runID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID, runWaitID uuid.UUID, runtimeCheckpointID uuid.UUID, workerCommandID int64) (bool, error) {
	if workerCommandID <= 0 {
		return false, nil
	}
	_, err := store.GetAcknowledgedReadyRuntimeCheckpointForRunWait(ctx, db.GetAcknowledgedReadyRuntimeCheckpointForRunWaitParams{
		OrgID:               pgvalue.UUID(orgID),
		RunID:               pgvalue.UUID(runID),
		RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
		RunWaitID:           pgvalue.UUID(runWaitID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		WorkerInstanceID:    pgvalue.UUID(workerInstanceID),
		WorkerCommandID:     workerCommandID,
	})
	if isNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    runWaitID.String(),
		CheckpointID: runtimeCheckpointID.String(),
	})
	return true, nil
}

func (s *Server) resolveReadyRunWait(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runWaitID pgtype.UUID) error {
	wait, err := store.GetRunWait(ctx, db.GetRunWaitParams{
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		ID:            runWaitID,
	})
	if err != nil {
		return err
	}
	switch wait.Kind {
	case db.RunWaitKindStream:
		streamWait, err := store.GetStreamWaitForRunWait(ctx, db.GetStreamWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     wait.ID,
		})
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = store.ResolveStreamWaitForRunWait(ctx, db.ResolveStreamWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     streamWait.RunWaitID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	case db.RunWaitKindToken:
		tokenWait, err := store.GetTokenWaitForRunWait(ctx, db.GetTokenWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     wait.ID,
		})
		if isNoRows(err) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = store.ResolveImmediateTokenWait(ctx, db.ResolveImmediateTokenWaitParams{OrgID: scope.OrgID, ID: tokenWait.ID})
		if isNoRows(err) {
			return nil
		}
		return err
	case db.RunWaitKindTimer:
		_, err := store.ResolveDueTimerWaitForRunWait(ctx, db.ResolveDueTimerWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     wait.ID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	default:
		return nil
	}
}

func acknowledgeCheckpointWorkerCommand(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, commandID int64, runID uuid.UUID, runWaitID uuid.UUID, runLeaseID uuid.UUID, workerInstanceID uuid.UUID) error {
	_, err := store.AcknowledgeWorkerCommandForRunWait(ctx, db.AcknowledgeWorkerCommandForRunWaitParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		ID:               commandID,
		OrgID:            scope.OrgID,
		RunID:            pgvalue.UUID(runID),
		RunWaitID:        pgvalue.UUID(runWaitID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		Kind:             db.WorkerCommandKindRuntimeCheckpointWait,
	})
	if isNoRows(err) {
		return conflict(errors.New("worker checkpoint command is not active for this run wait"))
	}
	if err != nil {
		return errors.New("acknowledge checkpoint worker command")
	}
	return nil
}

func (s *Server) workerMarkCheckpointFailed(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCheckpointFailedRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker runtime checkpoint failed request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	if request.WorkerCommandID <= 0 {
		writeError(w, badRequest(errors.New("worker_command_id must be positive")))
		return
	}
	err = s.inTx(r.Context(), func(work *txWork) error {
		scope, err := work.q.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
			OrgID:            pgvalue.UUID(orgID),
			RunID:            pgvalue.UUID(runID),
			RunLeaseID:       pgvalue.UUID(runLeaseID),
			WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
		})
		if isNoRows(err) {
			return conflict(errors.New("worker run lease is not active"))
		}
		if err != nil {
			return errors.New("load worker run wait scope")
		}
		if _, err := work.q.FailRuntimeCheckpointAttempt(r.Context(), db.FailRuntimeCheckpointAttemptParams{
			OrgID:               scope.OrgID,
			ProjectID:           scope.ProjectID,
			EnvironmentID:       scope.EnvironmentID,
			RunID:               pgvalue.UUID(runID),
			RunWaitID:           pgvalue.UUID(runWaitID),
			RunLeaseID:          pgvalue.UUID(runLeaseID),
			WorkerInstanceID:    pgvalue.UUID(worker.WorkerInstanceID),
			RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
			WorkerCommandID:     request.WorkerCommandID,
			ErrorMessage:        strings.TrimSpace(request.Error),
		}); isNoRows(err) {
			return conflict(errors.New("run wait is not parking for this run lease"))
		} else if err != nil {
			return errors.New("mark runtime checkpoint attempt failed")
		}
		return acknowledgeCheckpointWorkerCommand(r.Context(), work.q, scope, request.WorkerCommandID, runID, runWaitID, runLeaseID, worker.WorkerInstanceID)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerCheckpointResponse{
		RunID:        runID.String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: strings.TrimSpace(request.CheckpointID),
	})
}

func (s *Server) workerAcknowledgeRestore(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerAcknowledgeRestoreRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker restore ack request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	runWaitID, err := uuid.Parse(strings.TrimSpace(request.RunWaitID))
	if err != nil {
		writeError(w, badRequest(errors.New("run_wait_id must be a UUID")))
		return
	}
	runtimeCheckpointID, err := uuid.Parse(strings.TrimSpace(request.CheckpointID))
	if err != nil {
		writeError(w, badRequest(errors.New("checkpoint_id must be a UUID")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	workerLeaseIDs := workerRunLeaseIDs{
		orgID:           orgID,
		runID:           runID,
		runLeaseID:      runLeaseID,
		protocolVersion: strings.TrimSpace(request.Lease.ProtocolVersion),
		attemptNumber:   request.Lease.AttemptNumber,
		queueMessageID:  strings.TrimSpace(request.Lease.DispatchMessageID),
		queueLeaseID:    strings.TrimSpace(request.Lease.DispatchLeaseID),
	}
	if _, err := s.workerCurrentRunningLease(r.Context(), worker, workerLeaseIDs); isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	} else if err != nil {
		writeError(w, errors.New("load worker restore lease"))
		return
	}
	restorePhases, err := json.Marshal(request.Phases)
	if err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid restore phases: %w", err)))
		return
	}
	wait, err := s.db.MarkRuntimeResumeWaitResumed(r.Context(), db.MarkRuntimeResumeWaitResumedParams{
		OrgID:               pgvalue.UUID(orgID),
		ID:                  pgvalue.UUID(runWaitID),
		RunID:               pgvalue.UUID(runID),
		RunLeaseID:          pgvalue.UUID(runLeaseID),
		RuntimeCheckpointID: pgvalue.UUID(runtimeCheckpointID),
		RestorePhases:       restorePhases,
	})
	if err != nil && !isNoRows(err) {
		writeError(w, errors.New("acknowledge run wait restore"))
		return
	}
	if isNoRows(err) {
		writeError(w, conflict(errors.New("run wait is not ready for restore ack")))
		return
	}
	writeJSON(w, http.StatusOK, api.WorkerAcknowledgeRestoreResponse{
		RunID:        pgvalue.MustUUIDValue(wait.RunID).String(),
		RunWaitID:    strings.TrimSpace(request.RunWaitID),
		CheckpointID: strings.TrimSpace(request.CheckpointID),
	})
}

func (s *Server) workerCreateToken(w http.ResponseWriter, r *http.Request) {
	var request api.WorkerCreateTokenRequest
	if err := decodeJSON(r, &request); err != nil {
		writeError(w, badRequest(fmt.Errorf("invalid worker token request JSON: %w", err)))
		return
	}
	worker := workerFromContext(r.Context())
	if request.Lease.WorkerInstanceID != worker.WorkerInstanceID.String() {
		writeError(w, forbidden(errors.New("worker lease does not belong to this worker")))
		return
	}
	orgID, runID, runLeaseID, err := workerWaitLeaseIDs(request.Lease)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	scope, err := s.db.GetWorkerRunWaitScope(r.Context(), db.GetWorkerRunWaitScopeParams{
		OrgID:            pgvalue.UUID(orgID),
		RunID:            pgvalue.UUID(runID),
		RunLeaseID:       pgvalue.UUID(runLeaseID),
		WorkerInstanceID: pgvalue.UUID(worker.WorkerInstanceID),
	})
	if isNoRows(err) {
		writeError(w, conflict(errors.New("worker run lease is not active")))
		return
	}
	if err != nil {
		writeError(w, errors.New("load worker token scope"))
		return
	}
	timeout := json.RawMessage(`"7d"`)
	if request.TimeoutAt != nil {
		b, _ := json.Marshal(map[string]string{"date": request.TimeoutAt.UTC().Format(time.RFC3339Nano)})
		timeout = b
	} else if request.TimeoutInSeconds != nil {
		b, _ := json.Marshal(map[string]int32{"seconds": *request.TimeoutInSeconds})
		timeout = b
	}
	token, publicToken, err := s.createTokenRecord(r.Context(), s.db, auth.Actor{OrgID: orgID}, scope.ProjectID, scope.EnvironmentID, api.CreateTokenRequest{
		Timeout:  timeout,
		Tags:     request.Tags,
		Metadata: request.Metadata,
	})
	if err != nil {
		s.writeWorkerWaitError(w, err)
		return
	}
	row := tokenFromCreateRow(token)
	writeJSON(w, http.StatusOK, api.TokenResponse{
		ID:                pgvalue.MustUUIDValue(row.ID).String(),
		Status:            string(row.State),
		CallbackURL:       s.tokenCallbackURL(pgvalue.MustUUIDValue(row.ID)),
		PublicAccessToken: publicToken,
		TimeoutAt:         &row.TimeoutAt.Time,
		Tags:              row.Tags,
		Metadata:          json.RawMessage(row.Metadata),
	})
}

func workerWaitLeaseIDs(lease api.WorkerRunLease) (uuid.UUID, uuid.UUID, uuid.UUID, error) {
	orgID, err := uuid.Parse(strings.TrimSpace(lease.OrgID))
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, uuid.UUID{}, errors.New("lease.org_id must be a UUID")
	}
	runID, err := uuid.Parse(strings.TrimSpace(lease.RunID))
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, uuid.UUID{}, errors.New("lease.run_id must be a UUID")
	}
	runLeaseID, err := uuid.Parse(strings.TrimSpace(lease.ID))
	if err != nil {
		return uuid.UUID{}, uuid.UUID{}, uuid.UUID{}, errors.New("lease.id must be a UUID")
	}
	return orgID, runID, runLeaseID, nil
}

func workerWaitTimeoutAt(timeoutSeconds *int32) pgtype.Timestamptz {
	if timeoutSeconds == nil || *timeoutSeconds <= 0 {
		return pgtype.Timestamptz{}
	}
	return pgvalue.Timestamptz(time.Now().Add(time.Duration(*timeoutSeconds) * time.Second))
}

func (s *Server) writeWorkerWaitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errStreamNotFound):
		writeError(w, notFound(err))
	case errors.Is(err, errTokenNotFound):
		writeError(w, notFound(err))
	default:
		writeError(w, err)
	}
}

func validateWorkerCheckpointManifest(manifest api.WorkerCheckpointManifest) error {
	runtime := manifest.RecoveryPoint.Runtime
	required := map[string]string{
		"runtime.backend":          runtime.Backend,
		"runtime.id":               runtime.ID,
		"runtime.arch":             runtime.Arch,
		"runtime.abi":              runtime.ABI,
		"runtime.kernel_digest":    runtime.KernelDigest,
		"runtime.initramfs_digest": runtime.InitramfsDigest,
		"runtime.rootfs_digest":    runtime.RootfsDigest,
		"runtime.config_digest":    runtime.ConfigDigest,
	}
	for label, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if runtime.Substrate != nil {
		substrateRequired := map[string]string{
			"runtime.substrate.digest":      runtime.Substrate.Digest,
			"runtime.substrate.format":      runtime.Substrate.Format,
			"runtime.substrate.builder_abi": runtime.Substrate.BuilderABI,
			"runtime.substrate.layout_abi":  runtime.Substrate.LayoutABI,
		}
		for label, value := range substrateRequired {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("%s is required", label)
			}
		}
		if manifest.RuntimeState.RuntimeSubstrateArtifact == nil {
			return errors.New("runtime_state.runtime_substrate_artifact is required")
		}
		if err := validateWorkerRuntimeSubstrateArtifact("runtime_state.runtime_substrate_artifact", *manifest.RuntimeState.RuntimeSubstrateArtifact, *runtime.Substrate); err != nil {
			return err
		}
	}
	for label, artifact := range map[string]api.WorkerCheckpointArtifact{
		"runtime_state.config_artifact":       manifest.RuntimeState.ConfigArtifact,
		"runtime_state.vm_state_artifact":     manifest.RuntimeState.VMStateArtifact,
		"runtime_state.scratch_disk_artifact": manifest.RuntimeState.ScratchDiskArtifact,
	} {
		if err := validateWorkerCheckpointArtifact(label, artifact); err != nil {
			return err
		}
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		if err := validateWorkerCheckpointArtifact(fmt.Sprintf("runtime_state.memory_artifacts[%d]", index), artifact); err != nil {
			return err
		}
	}
	return nil
}

func checkpointSubstrateDigest(manifest api.WorkerCheckpointManifest) pgtype.Text {
	if manifest.RecoveryPoint.Runtime.Substrate == nil {
		return pgtype.Text{}
	}
	return pgvalue.Text(strings.TrimSpace(manifest.RecoveryPoint.Runtime.Substrate.Digest))
}

func checkpointRuntimeSubstrateArtifactID(manifest api.WorkerCheckpointManifest) (pgtype.UUID, error) {
	if manifest.RecoveryPoint.Runtime.Substrate == nil {
		return pgtype.UUID{}, nil
	}
	if manifest.RuntimeState.RuntimeSubstrateArtifact == nil {
		return pgtype.UUID{}, errors.New("runtime_state.runtime_substrate_artifact is required")
	}
	id, err := uuid.Parse(strings.TrimSpace(manifest.RuntimeState.RuntimeSubstrateArtifact.ID))
	if err != nil {
		return pgtype.UUID{}, errors.New("runtime_state.runtime_substrate_artifact.id must be a UUID")
	}
	return pgvalue.UUID(id), nil
}

func validateWorkerRuntimeSubstrateArtifact(label string, artifact api.WorkerRuntimeSubstrateArtifact, substrate api.WorkerCheckpointRuntimeSubstrate) error {
	required := map[string]string{
		label + ".id":                    artifact.ID,
		label + ".deployment_sandbox_id": artifact.DeploymentSandboxID,
		label + ".artifact.digest":       artifact.Artifact.Digest,
		label + ".artifact.media_type":   artifact.Artifact.MediaType,
		label + ".substrate_digest":      artifact.SubstrateDigest,
		label + ".format":                artifact.Format,
		label + ".builder_abi":           artifact.BuilderABI,
		label + ".layout_abi":            artifact.LayoutABI,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field)
		}
	}
	if strings.TrimSpace(artifact.Artifact.MediaType) != cas.RuntimeSubstrateMediaType {
		return fmt.Errorf("%s.artifact.media_type must be %s", label, cas.RuntimeSubstrateMediaType)
	}
	if artifact.Artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.artifact.size_bytes must be non-negative", label)
	}
	if artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must be non-negative", label)
	}
	if strings.TrimSpace(artifact.SubstrateDigest) != strings.TrimSpace(substrate.Digest) {
		return fmt.Errorf("%s.substrate_digest must match runtime.substrate.digest", label)
	}
	if strings.TrimSpace(artifact.Format) != strings.TrimSpace(substrate.Format) {
		return fmt.Errorf("%s.format must match runtime.substrate.format", label)
	}
	if strings.TrimSpace(artifact.BuilderABI) != strings.TrimSpace(substrate.BuilderABI) {
		return fmt.Errorf("%s.builder_abi must match runtime.substrate.builder_abi", label)
	}
	if strings.TrimSpace(artifact.LayoutABI) != strings.TrimSpace(substrate.LayoutABI) {
		return fmt.Errorf("%s.layout_abi must match runtime.substrate.layout_abi", label)
	}
	return nil
}

func validateWorkerCheckpointArtifact(label string, artifact api.WorkerCheckpointArtifact) error {
	if strings.TrimSpace(artifact.Digest) == "" {
		return fmt.Errorf("%s.digest is required", label)
	}
	if strings.TrimSpace(artifact.MediaType) == "" {
		return fmt.Errorf("%s.media_type is required", label)
	}
	if artifact.SizeBytes < 0 {
		return fmt.Errorf("%s.size_bytes must be non-negative", label)
	}
	return nil
}

type runtimeCheckpointArtifactIDs struct {
	config      pgtype.UUID
	vmState     pgtype.UUID
	scratchDisk pgtype.UUID
	memory      []pgtype.UUID
}

func (s *Server) createRuntimeCheckpointArtifacts(ctx context.Context, store db.Querier, workerInstanceID pgtype.UUID, scope db.GetWorkerRunWaitScopeRow, manifest api.WorkerCheckpointManifest) (runtimeCheckpointArtifactIDs, error) {
	config, err := createRuntimeCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.ConfigArtifact, db.ArtifactKindRuntimeCheckpointConfig)
	if err != nil {
		return runtimeCheckpointArtifactIDs{}, err
	}
	vmState, err := createRuntimeCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.VMStateArtifact, db.ArtifactKindRuntimeCheckpointVmState)
	if err != nil {
		return runtimeCheckpointArtifactIDs{}, err
	}
	scratchDisk, err := createRuntimeCheckpointArtifact(ctx, store, workerInstanceID, scope, manifest.RuntimeState.ScratchDiskArtifact, db.ArtifactKindRuntimeCheckpointScratchDisk)
	if err != nil {
		return runtimeCheckpointArtifactIDs{}, err
	}
	memory := make([]pgtype.UUID, 0, len(manifest.RuntimeState.MemoryArtifacts))
	for _, artifact := range manifest.RuntimeState.MemoryArtifacts {
		row, err := createRuntimeCheckpointArtifact(ctx, store, workerInstanceID, scope, artifact, db.ArtifactKindRuntimeCheckpointMemory)
		if err != nil {
			return runtimeCheckpointArtifactIDs{}, err
		}
		memory = append(memory, row.ID)
	}
	return runtimeCheckpointArtifactIDs{config: config.ID, vmState: vmState.ID, scratchDisk: scratchDisk.ID, memory: memory}, nil
}

func createRuntimeCheckpointArtifact(ctx context.Context, store db.Querier, workerInstanceID pgtype.UUID, scope db.GetWorkerRunWaitScopeRow, artifact api.WorkerCheckpointArtifact, kind db.ArtifactKind) (db.Artifact, error) {
	if _, err := store.UpsertCasObject(ctx, db.UpsertCasObjectParams{
		Digest:    artifact.Digest,
		SizeBytes: artifact.SizeBytes,
		MediaType: artifact.MediaType,
	}); err != nil {
		return db.Artifact{}, err
	}
	return store.CreateArtifact(ctx, db.CreateArtifactParams{
		ID:                        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:                     scope.OrgID,
		ProjectID:                 scope.ProjectID,
		EnvironmentID:             scope.EnvironmentID,
		Digest:                    artifact.Digest,
		Kind:                      kind,
		SizeBytes:                 artifact.SizeBytes,
		MediaType:                 artifact.MediaType,
		CreatedByWorkerInstanceID: workerInstanceID,
	})
}

func (s *Server) createRuntimeCheckpointArtifactRows(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runtimeCheckpointID pgtype.UUID, manifest api.WorkerCheckpointManifest, artifacts runtimeCheckpointArtifactIDs) error {
	rows := []struct {
		role     db.RuntimeCheckpointArtifactRole
		ordinal  int32
		id       pgtype.UUID
		artifact api.WorkerCheckpointArtifact
	}{
		{role: db.RuntimeCheckpointArtifactRoleRuntimeConfig, id: artifacts.config, artifact: manifest.RuntimeState.ConfigArtifact},
		{role: db.RuntimeCheckpointArtifactRoleVmState, id: artifacts.vmState, artifact: manifest.RuntimeState.VMStateArtifact},
		{role: db.RuntimeCheckpointArtifactRoleScratchDisk, id: artifacts.scratchDisk, artifact: manifest.RuntimeState.ScratchDiskArtifact},
	}
	for index, artifact := range manifest.RuntimeState.MemoryArtifacts {
		rows = append(rows, struct {
			role     db.RuntimeCheckpointArtifactRole
			ordinal  int32
			id       pgtype.UUID
			artifact api.WorkerCheckpointArtifact
		}{role: db.RuntimeCheckpointArtifactRoleMemory, ordinal: int32(index), id: artifacts.memory[index], artifact: artifact})
	}
	for _, row := range rows {
		if _, err := store.CreateRuntimeCheckpointArtifact(ctx, db.CreateRuntimeCheckpointArtifactParams{
			Role:                row.role,
			Ordinal:             row.ordinal,
			EncryptDurationMs:   row.artifact.EncryptDurationMs,
			StoreDurationMs:     row.artifact.StoreDurationMs,
			ArtifactID:          row.id,
			Digest:              row.artifact.Digest,
			OrgID:               scope.OrgID,
			ProjectID:           scope.ProjectID,
			EnvironmentID:       scope.EnvironmentID,
			RunID:               scope.RunID,
			RuntimeCheckpointID: runtimeCheckpointID,
		}); err != nil {
			return err
		}
	}
	return nil
}
