package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	ErrNoEnqueueCandidate = errors.New("no queue candidate")
	ErrStaleEnqueueHint   = errors.New("stale queue hint")
)

type EnqueuerStore interface {
	GetQueuedRunReadyHint(context.Context, db.GetQueuedRunReadyHintParams) (db.GetQueuedRunReadyHintRow, error)
	GetQueuedRunResumeHint(context.Context, db.GetQueuedRunResumeHintParams) (db.GetQueuedRunResumeHintRow, error)
	GetRunResumeHintAuthority(context.Context, db.GetRunResumeHintAuthorityParams) (db.GetRunResumeHintAuthorityRow, error)
	ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error)
	ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error)
	ListQueuedDeploymentBuildRegions(context.Context, int32) ([]string, error)
}

func (e *Enqueuer) EnqueueRunResume(
	ctx context.Context,
	environmentID pgtype.UUID,
	runID pgtype.UUID,
	runWaitID pgtype.UUID,
	resumeRequestVersion int64,
) (EnqueueResult, error) {
	row, err := e.store.GetQueuedRunResumeHint(ctx, db.GetQueuedRunResumeHintParams{
		EnvironmentID:        environmentID,
		RunID:                runID,
		RunWaitID:            runWaitID,
		ResumeRequestVersion: resumeRequestVersion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		authority, authorityErr := e.store.GetRunResumeHintAuthority(ctx, db.GetRunResumeHintAuthorityParams{
			EnvironmentID: environmentID,
			RunID:         runID,
			RunWaitID:     runWaitID,
		})
		if errors.Is(authorityErr, pgx.ErrNoRows) {
			return EnqueueResult{}, ErrStaleEnqueueHint
		}
		if authorityErr != nil {
			return EnqueueResult{}, authorityErr
		}
		if authority.Status == db.RunStatusQueued && !authority.CurrentRunLeaseID.Valid &&
			authority.ConditionState != db.WaitStatePending &&
			authority.SuspensionState == db.RunWaitStateResumePending &&
			authority.ExpectedRunStateVersion == authority.StateVersion &&
			authority.ResumeRequestVersion == resumeRequestVersion {
			return EnqueueResult{}, ErrNoEnqueueCandidate
		}
		return EnqueueResult{}, ErrStaleEnqueueHint
	}
	if err != nil {
		return EnqueueResult{}, err
	}
	message, err := queueResumeMessage(row)
	if err != nil {
		return EnqueueResult{}, err
	}
	result, err := e.queue.Enqueue(ctx, message)
	if err != nil {
		return EnqueueResult{}, err
	}
	return result, nil
}

func (e *Enqueuer) ReconcileBuildReady(ctx context.Context, regionLimit, candidateLimit int32) (QueueReconcileStats, error) {
	regions, err := e.store.ListQueuedDeploymentBuildRegions(ctx, regionLimit)
	if err != nil {
		return QueueReconcileStats{}, err
	}
	var stats QueueReconcileStats
	var problems []error
	remaining := candidateLimit
	for _, region := range regions {
		if remaining <= 0 {
			break
		}
		rows, err := e.store.ListQueuedDeploymentBuildCandidates(ctx, db.ListQueuedDeploymentBuildCandidatesParams{BuildRegionID: region, LimitCount: remaining})
		if err != nil {
			problems = append(problems, err)
			continue
		}
		stats.Scanned += len(rows)
		for _, row := range rows {
			message, err := buildQueueMessage(row)
			if err == nil {
				_, err = e.queue.Enqueue(ctx, message)
			}
			if err != nil {
				stats.Failed++
				problems = append(problems, err)
			} else {
				stats.Enqueued++
			}
			remaining--
		}
	}
	return stats, errors.Join(problems...)
}

func buildQueueMessage(row db.ListQueuedDeploymentBuildCandidatesRow) (Message, error) {
	deploymentID, err := pgUUIDString(row.DeploymentID)
	if err != nil {
		return Message{}, err
	}
	orgID, err := pgUUIDString(row.OrgID)
	if err != nil {
		return Message{}, err
	}
	projectID, err := pgUUIDString(row.ProjectID)
	if err != nil {
		return Message{}, err
	}
	environmentID, err := pgUUIDString(row.EnvironmentID)
	if err != nil {
		return Message{}, err
	}
	message := Message{WorkKind: WorkKindBuild, DeploymentID: deploymentID, OrgID: orgID,
		ProjectID: projectID, EnvironmentID: environmentID, RegionID: row.BuildRegionID,
		QueueName: "deployment-build", LeaseSequence: row.LeaseSequence,
		QueueOriginAt: row.QueueTimestamp.Time, QueueScoreAt: row.QueueTimestamp.Time, EnqueuedAt: time.Now().UTC(),
		BuildArchitecture: row.BuildArchitecture,
		BuildResources: BuildResourceVector{CPUMillis: row.BuildRequestedCpuMillis, MemoryBytes: row.BuildRequestedMemoryBytes,
			WorkloadDiskBytes: row.BuildRequestedWorkloadDiskBytes, ScratchBytes: row.BuildRequestedScratchBytes,
			Executors: row.BuildRequestedExecutors}}
	if err := message.Validate(); err != nil {
		return Message{}, err
	}
	return message, nil
}

type Enqueuer struct {
	store     EnqueuerStore
	queue     Queue
	errorSize int
}

type EnqueuerOption func(*Enqueuer)

func NewEnqueuer(store EnqueuerStore, queue Queue, opts ...EnqueuerOption) (*Enqueuer, error) {
	if store == nil {
		return nil, errors.New("queue store is required")
	}
	if queue == nil {
		return nil, errors.New("queue is required")
	}
	e := &Enqueuer{
		store:     store,
		queue:     queue,
		errorSize: 1024,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.errorSize <= 0 {
		return nil, errors.New("enqueue error size must be positive")
	}
	return e, nil
}

func (e *Enqueuer) EnqueueRun(ctx context.Context, orgID pgtype.UUID, runID pgtype.UUID) (EnqueueResult, error) {
	row, err := e.store.GetQueuedRunReadyHint(ctx, db.GetQueuedRunReadyHintParams{
		OrgID: orgID,
		RunID: runID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EnqueueResult{}, ErrNoEnqueueCandidate
	}
	if err != nil {
		return EnqueueResult{}, err
	}
	message, err := queueMessage(row)
	if err != nil {
		return EnqueueResult{}, err
	}
	result, err := e.queue.Enqueue(ctx, message)
	if err != nil {
		return EnqueueResult{}, err
	}
	return result, nil
}

type QueueReconcileStats struct {
	Scanned  int
	Enqueued int
	Skipped  int
	Failed   int
}

func (e *Enqueuer) ReconcileQueueScope(ctx context.Context, scope QueueScope, limit int32) (QueueReconcileStats, error) {
	if limit <= 0 {
		limit = 100
	}
	candidates, err := e.store.ListQueuedRunDispatchCandidatesForScope(ctx, db.ListQueuedRunDispatchCandidatesForScopeParams{
		OrgID:          scope.OrgID,
		RegionID:       scope.RegionID,
		ProjectID:      scope.ProjectID,
		EnvironmentID:  scope.EnvironmentID,
		ConcurrencyKey: pgtype.Text{String: scope.ConcurrencyKey, Valid: scope.ConcurrencyKey != ""},
		QueueName:      scope.QueueName,
		RowLimit:       limit,
	})
	if err != nil {
		return QueueReconcileStats{}, err
	}
	stats := QueueReconcileStats{Scanned: len(candidates)}
	var problems []error
	for _, candidate := range candidates {
		if _, err := e.EnqueueRun(ctx, candidate.OrgID, candidate.RunID); err != nil {
			if errors.Is(err, ErrNoEnqueueCandidate) {
				stats.Skipped++
				continue
			}
			stats.Failed++
			problems = append(problems, err)
			continue
		}
		stats.Enqueued++
	}
	return stats, errors.Join(problems...)
}

func queueMessage(row db.GetQueuedRunReadyHintRow) (Message, error) {
	return newQueueMessage(
		row.ID, row.OrgID, row.ProjectID, row.EnvironmentID, row.RegionID,
		row.QueueName, row.ConcurrencyKey, row.StateVersion, row.Priority,
		row.QueueOriginAt.Time, row.QueueScoreAt.Time,
	)
}

func queueResumeMessage(row db.GetQueuedRunResumeHintRow) (Message, error) {
	return newQueueMessage(
		row.ID, row.OrgID, row.ProjectID, row.EnvironmentID, row.RegionID,
		row.QueueName, row.ConcurrencyKey, row.StateVersion, row.Priority,
		row.QueueOriginAt.Time, row.QueueScoreAt.Time,
	)
}

func newQueueMessage(
	runIDValue pgtype.UUID,
	orgIDValue pgtype.UUID,
	projectIDValue pgtype.UUID,
	environmentIDValue pgtype.UUID,
	regionID string,
	queueName string,
	concurrencyKey pgtype.Text,
	runStateVersion int64,
	priority int32,
	queueOriginAt time.Time,
	queueScoreAt time.Time,
) (Message, error) {
	runID, err := pgUUIDString(runIDValue)
	if err != nil {
		return Message{}, fmt.Errorf("run id: %w", err)
	}
	orgID, err := pgUUIDString(orgIDValue)
	if err != nil {
		return Message{}, fmt.Errorf("org id: %w", err)
	}
	projectID, err := pgUUIDString(projectIDValue)
	if err != nil {
		return Message{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := pgUUIDString(environmentIDValue)
	if err != nil {
		return Message{}, fmt.Errorf("environment id: %w", err)
	}
	return Message{
		WorkKind:        WorkKindRun,
		RunID:           runID,
		OrgID:           orgID,
		RegionID:        regionID,
		ProjectID:       projectID,
		EnvironmentID:   environmentID,
		QueueName:       queueName,
		ConcurrencyKey:  concurrencyKey.String,
		RunStateVersion: runStateVersion,
		Priority:        priority,
		QueueOriginAt:   queueOriginAt,
		QueueScoreAt:    queueScoreAt,
		EnqueuedAt:      time.Now().UTC(),
	}, nil
}

func pgUUIDString(value pgtype.UUID) (string, error) {
	parsed, err := pgvalue.UUIDValue(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}
