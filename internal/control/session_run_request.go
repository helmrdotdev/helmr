package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/tracing"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	sessionRunRequestClaimTTL       = 30 * time.Second
	sessionRunRequestReconcileEvery = time.Second
	sessionRunRequestClaimLimit     = int32(10)
)

var errSessionRunRequestLost = errors.New("session run request claim lost")

func (s *Server) reconcileAcceptedSessionRunRequests(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, sessionID pgtype.UUID) []pgtype.UUID {
	if s.db == nil {
		return nil
	}
	return s.reconcileDueSessionRunRequests(ctx, orgID, projectID, environmentID, sessionID, sessionRunRequestClaimLimit)
}

func (s *Server) RunSessionRunRequestReconciler(ctx context.Context) {
	ticker := time.NewTicker(sessionRunRequestReconcileEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileDueSessionRunRequests(ctx, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}, sessionRunRequestClaimLimit)
		}
	}
}

func (s *Server) reconcileDueSessionRunRequests(ctx context.Context, orgID pgtype.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, sessionID pgtype.UUID, limit int32) []pgtype.UUID {
	claimOwner := "control:" + uuid.Must(uuid.NewV7()).String()
	requests, err := s.db.ClaimDueSessionRunRequests(ctx, db.ClaimDueSessionRunRequestsParams{
		ClaimTtl:      pgvalue.Interval(sessionRunRequestClaimTTL),
		ClaimOwner:    claimOwner,
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		SessionID:     sessionID,
		LimitCount:    limit,
	})
	if err != nil {
		s.log.Error("claim due session run requests failed", "session_id", pgvalue.UUIDString(sessionID), "error", err)
		return nil
	}
	runIDs := make([]pgtype.UUID, 0, len(requests))
	for _, request := range requests {
		runID, err := s.reconcileClaimedSessionRunRequest(ctx, request)
		if err != nil {
			s.log.Error("reconcile session run request failed", "request_id", pgvalue.MustUUIDValue(request.ID).String(), "error", err)
			continue
		}
		if runID.Valid {
			runIDs = append(runIDs, runID)
			s.enqueueContinuationRun(ctx, request.OrgID, runID)
		}
	}
	return runIDs
}

func (s *Server) reconcileClaimedSessionRunRequest(ctx context.Context, request db.SessionRunRequest) (pgtype.UUID, error) {
	session := db.Session{
		ID:            request.SessionID,
		OrgID:         request.OrgID,
		ProjectID:     request.ProjectID,
		EnvironmentID: request.EnvironmentID,
	}
	var runID pgtype.UUID
	err := s.inTx(ctx, func(work *txWork) error {
		record, err := work.q.GetStreamRecord(ctx, db.GetStreamRecordParams{
			OrgID:         request.OrgID,
			ProjectID:     request.ProjectID,
			EnvironmentID: request.EnvironmentID,
			ID:            request.StreamRecordID,
		})
		if isNoRows(err) {
			if _, markErr := work.q.MarkSessionRunRequestFailed(ctx, db.MarkSessionRunRequestFailedParams{
				OrgID:         request.OrgID,
				ProjectID:     request.ProjectID,
				EnvironmentID: request.EnvironmentID,
				ID:            request.ID,
				ClaimOwner:    request.ClaimOwner,
				Reason:        "stream_record_not_found",
			}); markErr != nil {
				return markErr
			}
			return nil
		}
		if err != nil {
			if retryErr := s.releaseSessionRunRequestForRetry(ctx, work.q, request, err.Error(), sessionRunRequestRetryAfter(request, "")); retryErr != nil {
				return retryErr
			}
			return nil
		}
		createdRunID, status, err := s.tryCreateContinuationRunForRequest(ctx, work.q, session, request, record)
		if err != nil {
			if errors.Is(err, errSessionRunRequestLost) {
				return err
			}
			if retryErr := s.releaseSessionRunRequestForRetry(ctx, work.q, request, err.Error(), sessionRunRequestRetryAfter(request, status)); retryErr != nil {
				return retryErr
			}
			return nil
		}
		if status == "accepted_run_pending" {
			if err := s.releaseSessionRunRequestForRetry(ctx, work.q, request, "current_run_not_terminal", sessionRunRequestRetryAfter(request, status)); err != nil {
				return err
			}
		}
		runID = createdRunID
		return nil
	})
	if err != nil {
		return pgtype.UUID{}, err
	}
	return runID, nil
}

func (s *Server) releaseSessionRunRequestForRetry(ctx context.Context, store db.Querier, request db.SessionRunRequest, lastError string, retryAfter time.Duration) error {
	_, err := store.ReleaseSessionRunRequestForRetry(ctx, db.ReleaseSessionRunRequestForRetryParams{
		RetryAfter:    pgvalue.Interval(retryAfter),
		LastError:     lastError,
		OrgID:         request.OrgID,
		ProjectID:     request.ProjectID,
		EnvironmentID: request.EnvironmentID,
		ID:            request.ID,
		ClaimOwner:    request.ClaimOwner,
	})
	return err
}

func sessionRunRequestRetryAfter(request db.SessionRunRequest, status string) time.Duration {
	if status == "accepted_run_pending" {
		return time.Second
	}
	switch {
	case request.Attempts <= 1:
		return 250 * time.Millisecond
	case request.Attempts == 2:
		return time.Second
	case request.Attempts == 3:
		return 5 * time.Second
	default:
		return 30 * time.Second
	}
}

func (s *Server) tryCreateContinuationRunForRequest(ctx context.Context, store db.Querier, session db.Session, request db.SessionRunRequest, record db.StreamRecord) (pgtype.UUID, string, error) {
	if request.Status == "created" && request.RunID.Valid {
		return request.RunID, "duplicate", nil
	}
	if request.Status != "accepted" && request.Status != "claimed" {
		return pgtype.UUID{}, string(request.Status), nil
	}
	locked, err := store.LockSession(ctx, db.LockSessionParams{
		OrgID:         session.OrgID,
		ProjectID:     session.ProjectID,
		EnvironmentID: session.EnvironmentID,
		ID:            session.ID,
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	if locked.Status != db.SessionStatusOpen {
		reason := "session_not_open"
		if locked.Status == db.SessionStatusExpired {
			reason = "session_expired"
		}
		if _, err := store.MarkSessionRunRequestSkipped(ctx, db.MarkSessionRunRequestSkippedParams{
			OrgID:         request.OrgID,
			ProjectID:     request.ProjectID,
			EnvironmentID: request.EnvironmentID,
			ID:            request.ID,
			ClaimOwner:    request.ClaimOwner,
			Reason:        reason,
		}); err != nil {
			return pgtype.UUID{}, "", err
		}
		return pgtype.UUID{}, "skipped", nil
	}
	if locked.ExpiresAt.Valid && !locked.ExpiresAt.Time.After(time.Now()) {
		if _, err := store.MarkSessionRunRequestSkipped(ctx, db.MarkSessionRunRequestSkippedParams{
			OrgID:         request.OrgID,
			ProjectID:     request.ProjectID,
			EnvironmentID: request.EnvironmentID,
			ID:            request.ID,
			ClaimOwner:    request.ClaimOwner,
			Reason:        "session_expired",
		}); err != nil {
			return pgtype.UUID{}, "", err
		}
		return pgtype.UUID{}, "skipped", nil
	}
	if !locked.CurrentRunID.Valid {
		if _, err := store.MarkSessionRunRequestFailed(ctx, db.MarkSessionRunRequestFailedParams{
			OrgID:         request.OrgID,
			ProjectID:     request.ProjectID,
			EnvironmentID: request.EnvironmentID,
			ID:            request.ID,
			ClaimOwner:    request.ClaimOwner,
			Reason:        "session_has_no_current_run",
		}); err != nil {
			return pgtype.UUID{}, "", err
		}
		return pgtype.UUID{}, "failed", nil
	}
	previousRun, err := store.GetRun(ctx, db.GetRunParams{OrgID: locked.OrgID, ID: locked.CurrentRunID})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	if !runStatusTerminal(previousRun.Status) {
		return pgtype.UUID{}, "accepted_run_pending", nil
	}
	previousSessionRun, err := store.GetSessionRunByRunID(ctx, db.GetSessionRunByRunIDParams{
		OrgID:         locked.OrgID,
		ProjectID:     locked.ProjectID,
		EnvironmentID: locked.EnvironmentID,
		SessionID:     locked.ID,
		RunID:         locked.CurrentRunID,
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	deploymentTask, err := store.GetDeploymentTask(ctx, db.GetDeploymentTaskParams{
		OrgID:         locked.OrgID,
		ProjectID:     locked.ProjectID,
		EnvironmentID: locked.EnvironmentID,
		DeploymentID:  locked.ActiveDeploymentID,
		TaskID:        locked.TaskID,
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	maxDurationSeconds, err := runMaxDurationSeconds(0, deploymentTask.MaxActiveDurationMs)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	lockedRetryPolicy, err := resolvedRetryPolicy(nil, deploymentTask.RetryPolicy)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	scheduling, err := s.resolveRunScheduling(api.CreateRunOptions{}, deploymentTask)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	traceID, err := tracing.NewTraceID()
	if err != nil {
		return pgtype.UUID{}, "", fmt.Errorf("generate continuation run trace id: %w", err)
	}
	rootSpanID, err := tracing.NewSpanID()
	if err != nil {
		return pgtype.UUID{}, "", fmt.Errorf("generate continuation run root span id: %w", err)
	}
	cause, err := json.Marshal(map[string]any{
		"kind":      "stream_record",
		"record_id": pgvalue.MustUUIDValue(record.ID).String(),
		"stream_id": pgvalue.MustUUIDValue(record.StreamID).String(),
		"sequence":  record.Sequence,
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	secretNames, err := deploymentTaskSecretNames(deploymentTask.SecretDeclarations)
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	createdPayload, err := runCreatedEventPayload(locked.TaskID, previousRun.Payload, maxDurationSeconds, secretNames, lockedRetryPolicy, locked.Metadata, locked.Tags, "input", cause)
	if err != nil {
		return pgtype.UUID{}, "", fmt.Errorf("encode continuation run created event: %w", err)
	}
	runID := pgvalue.UUID(uuid.Must(uuid.NewV7()))
	run, err := store.CreateScopedRun(ctx, db.CreateScopedRunParams{
		ID:                    runID,
		OrgID:                 locked.OrgID,
		ProjectID:             locked.ProjectID,
		EnvironmentID:         locked.EnvironmentID,
		DeploymentID:          deploymentTask.DeploymentID,
		DeploymentTaskID:      deploymentTask.ID,
		WorkspaceID:           locked.WorkspaceID,
		DeploymentVersion:     deploymentTask.DeploymentVersion,
		ApiVersion:            deploymentTask.ApiVersion,
		SdkVersion:            deploymentTask.SdkVersion,
		CliVersion:            deploymentTask.CliVersion,
		TaskID:                locked.TaskID,
		SessionID:             locked.ID,
		Payload:               previousRun.Payload,
		Metadata:              locked.Metadata,
		Tags:                  locked.Tags,
		LockedRetryPolicy:     lockedRetryPolicy,
		QueueName:             scheduling.queueName,
		QueueConcurrencyLimit: scheduling.queueConcurrencyLimit,
		ConcurrencyKey:        scheduling.concurrencyKey,
		Priority:              scheduling.priority,
		QueueTimestamp:        scheduling.queueTimestamp,
		Ttl:                   scheduling.ttl,
		QueuedExpiresAt:       scheduling.queuedExpiresAt,
		MaxActiveDurationMs:   int64(maxDurationSeconds) * 1000,
		TraceID:               traceID,
		RootSpanID:            rootSpanID,
		EventPayload:          createdPayload,
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	workspaceMountRequest, err := json.Marshal(map[string]string{
		"source": "session_input",
		"run_id": pgvalue.MustUUIDValue(run.ID).String(),
	})
	if err != nil {
		return pgtype.UUID{}, "", err
	}
	mount, err := store.EnsureWorkspaceMountRequested(ctx, db.EnsureWorkspaceMountRequestedParams{
		ID:              pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:           locked.OrgID,
		ProjectID:       locked.ProjectID,
		EnvironmentID:   locked.EnvironmentID,
		WorkspaceID:     locked.WorkspaceID,
		RequestPriority: scheduling.priority,
		Request:         workspaceMountRequest,
	})
	if err != nil {
		if isNoRows(err) {
			return pgtype.UUID{}, "", s.workspaceMountPrerequisiteErrorWithStore(ctx, store, locked.OrgID, locked.ProjectID, locked.EnvironmentID, locked.WorkspaceID)
		}
		return pgtype.UUID{}, "", err
	}
	if err := store.SetQueuedRunWorkspaceMount(ctx, db.SetQueuedRunWorkspaceMountParams{
		OrgID:            locked.OrgID,
		RunID:            run.ID,
		WorkspaceID:      locked.WorkspaceID,
		WorkspaceMountID: mount.ID,
	}); err != nil {
		return pgtype.UUID{}, "", err
	}
	if _, err := store.CreateSessionRun(ctx, db.CreateSessionRunParams{
		ID:            pgvalue.UUID(uuid.Must(uuid.NewV7())),
		OrgID:         locked.OrgID,
		ProjectID:     locked.ProjectID,
		EnvironmentID: locked.EnvironmentID,
		SessionID:     locked.ID,
		RunID:         run.ID,
		DeploymentID:  deploymentTask.DeploymentID,
		PreviousRunID: locked.CurrentRunID,
		TurnIndex:     previousSessionRun.TurnIndex + 1,
		Reason:        "input",
	}); err != nil {
		return pgtype.UUID{}, "", err
	}
	if _, err := store.SetSessionCurrentRun(ctx, db.SetSessionCurrentRunParams{
		OrgID:         locked.OrgID,
		ProjectID:     locked.ProjectID,
		EnvironmentID: locked.EnvironmentID,
		SessionID:     locked.ID,
		RunID:         run.ID,
	}); err != nil {
		return pgtype.UUID{}, "", err
	}
	if _, err := store.MarkSessionRunRequestCreated(ctx, db.MarkSessionRunRequestCreatedParams{
		OrgID:         request.OrgID,
		ProjectID:     request.ProjectID,
		EnvironmentID: request.EnvironmentID,
		ID:            request.ID,
		ClaimOwner:    request.ClaimOwner,
		RunID:         run.ID,
	}); err != nil {
		if isNoRows(err) {
			return pgtype.UUID{}, "", errSessionRunRequestLost
		}
		return pgtype.UUID{}, "", err
	}
	return run.ID, "created", nil
}

func (s *Server) consumeSessionRunRequestByActiveRun(ctx context.Context, session db.Session, activeRunID pgtype.UUID, streamRecordID pgtype.UUID) error {
	return s.inTx(ctx, func(work *txWork) error {
		if _, err := work.q.LockSession(ctx, db.LockSessionParams{
			OrgID:         session.OrgID,
			ProjectID:     session.ProjectID,
			EnvironmentID: session.EnvironmentID,
			ID:            session.ID,
		}); err != nil {
			return err
		}
		if _, err := work.q.MarkSessionRunRequestConsumedByActiveRun(ctx, db.MarkSessionRunRequestConsumedByActiveRunParams{
			OrgID:          session.OrgID,
			ProjectID:      session.ProjectID,
			EnvironmentID:  session.EnvironmentID,
			ActiveRunID:    activeRunID,
			StreamRecordID: streamRecordID,
		}); err != nil && !isNoRows(err) {
			return err
		}
		return nil
	})
}

func runStatusTerminal(status db.RunStatus) bool {
	switch status {
	case db.RunStatusSucceeded, db.RunStatusFailed, db.RunStatusCancelled, db.RunStatusExpired:
		return true
	default:
		return false
	}
}

func (s *Server) enqueueContinuationRun(ctx context.Context, orgID pgtype.UUID, runID pgtype.UUID) {
	if s.runEnqueuer == nil {
		return
	}
	if _, err := s.runEnqueuer.EnqueueRun(ctx, orgID, runID); err != nil {
		s.log.Error("enqueue continuation run failed", "run_id", pgvalue.MustUUIDValue(runID).String(), "error", err)
	}
}
