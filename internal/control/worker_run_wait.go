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
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/jackc/pgx/v5/pgtype"
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

var errWorkerTokenWaitResolvedRollback = errors.New("worker token wait resolved inline")

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
	correlationKey := strings.TrimSpace(request.CorrelationID)
	var streamID pgtype.UUID
	var streamSequence pgtype.Int8
	var tokenID pgtype.UUID
	var completedAfter pgtype.Timestamptz
	var expiresAt pgtype.Timestamptz
	switch request.Kind {
	case api.WorkerRunWaitKindStream:
		params, stream, err := s.workerInputStreamWaitTarget(ctx, s.db, scope, request)
		if err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		streamID = stream.ID
		streamSequence = pgtype.Int8{Int64: params.AfterSequence, Valid: true}
		if params.CorrelationID != "" {
			correlationKey = strings.TrimSpace(params.CorrelationID)
		}
		expiresAt = workerWaitTimeoutAt(request.TimeoutSeconds)
	case api.WorkerRunWaitKindToken:
		parsedTokenID, err := workerTokenWaitTokenID(request)
		if err != nil {
			return api.WorkerCreateRunWaitResponse{}, err
		}
		tokenID = pgvalue.UUID(parsedTokenID)
		expiresAt = workerWaitTimeoutAt(request.TimeoutSeconds)
	case api.WorkerRunWaitKindTimer:
		completedAfter = pgvalue.Timestamptz(time.Now().Add(time.Duration(*request.TimeoutSeconds) * time.Second))
	}
	runWaitID := uuid.Must(uuid.NewV7())
	waitID := uuid.Must(uuid.NewV7())
	waitPolicy := selectWorkerRunWaitPolicy(request)
	var response api.WorkerCreateRunWaitResponse
	err := s.inTx(ctx, func(work *txWork) error {
		var publicID string
		createdRunWait, err := createWithPublicID(ctx, []publicIDSlot{{prefix: publicid.Wait, value: &publicID}}, func() (db.CreateHotRunWaitRow, error) {
			return work.q.CreateHotRunWait(ctx, db.CreateHotRunWaitParams{
				RunWaitID:        pgvalue.UUID(runWaitID),
				WaitID:           pgvalue.UUID(waitID),
				PublicID:         publicID,
				OrgID:            scope.OrgID,
				ProjectID:        scope.ProjectID,
				EnvironmentID:    scope.EnvironmentID,
				RunID:            scope.RunID,
				RunLeaseID:       scope.CurrentRunLeaseID,
				WorkerInstanceID: scope.WorkerInstanceID,
				Kind:             db.WaitKind(request.Kind),
				CorrelationKey:   correlationKey,
				StreamID:         streamID,
				StreamSequence:   streamSequence,
				TokenID:          tokenID,
				CompletedAfter:   completedAfter,
				ExpiresAt:        expiresAt,
				CheckpointDelay:  pgvalue.Interval(waitPolicy.CheckpointDelay),
			})
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
		if request.Kind == api.WorkerRunWaitKindToken {
			tokenResolved := true
			_, err := work.q.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
				OrgID:         scope.OrgID,
				WorkerGroupID: scope.WorkerGroupID,
				ProjectID:     scope.ProjectID,
				EnvironmentID: scope.EnvironmentID,
				RunWaitID:     runWait.ID,
			})
			if isNoRows(err) {
				err = nil
				tokenResolved = false
			}
			if err != nil {
				return err
			}
			if tokenResolved {
				token, tokenErr := work.q.GetToken(ctx, db.GetTokenParams{
					OrgID:         scope.OrgID,
					ProjectID:     scope.ProjectID,
					EnvironmentID: scope.EnvironmentID,
					ID:            tokenID,
				})
				if tokenErr != nil {
					return tokenErr
				}
				resolutionKind, resolution, matched, resolutionErr := workerTokenResolution(token)
				if resolutionErr != nil {
					return resolutionErr
				}
				if matched {
					response = api.WorkerCreateRunWaitResponse{
						RunID:          pgvalue.MustUUIDValue(scope.RunID).String(),
						ResolutionKind: resolutionKind,
						Resolution:     resolution,
					}
					return errWorkerTokenWaitResolvedRollback
				}
			}
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
			return nil
		case api.WorkerRunWaitKindToken:
			return nil
		case api.WorkerRunWaitKindTimer:
			return nil
		}
		return nil
	})
	if err == errWorkerTokenWaitResolvedRollback {
		return response, nil
	}
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

func (s *Server) resolveReadyRunWait(ctx context.Context, store db.Querier, scope db.GetWorkerRunWaitScopeRow, runWaitID pgtype.UUID) error {
	wait, err := store.GetWaitForRunWait(ctx, db.GetWaitForRunWaitParams{
		OrgID:         scope.OrgID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		RunWaitID:     runWaitID,
	})
	if err != nil {
		return err
	}
	switch wait.Kind {
	case db.WaitKindStream:
		_, err = store.ResolveStreamWaitForRunWait(ctx, db.ResolveStreamWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			WorkerGroupID: scope.WorkerGroupID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     runWaitID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	case db.WaitKindToken:
		_, err = store.ResolveImmediateTokenWaitForRunWait(ctx, db.ResolveImmediateTokenWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			WorkerGroupID: scope.WorkerGroupID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     runWaitID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	case db.WaitKindTimer:
		_, err := store.ResolveDueTimerWaitForRunWait(ctx, db.ResolveDueTimerWaitForRunWaitParams{
			OrgID:         scope.OrgID,
			ProjectID:     scope.ProjectID,
			EnvironmentID: scope.EnvironmentID,
			RunWaitID:     runWaitID,
		})
		if isNoRows(err) {
			return nil
		}
		return err
	default:
		return nil
	}
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
