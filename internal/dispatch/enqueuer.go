package dispatch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNoEnqueueCandidate = errors.New("no queue candidate")

type EnqueuerStore interface {
	PrepareQueuedRunDispatch(context.Context, db.PrepareQueuedRunDispatchParams) (db.PrepareQueuedRunDispatchRow, error)
	ListQueuedRunDispatchCandidatesForScope(context.Context, db.ListQueuedRunDispatchCandidatesForScopeParams) ([]db.ListQueuedRunDispatchCandidatesForScopeRow, error)
	ListQueuedDeploymentBuildCandidates(context.Context, db.ListQueuedDeploymentBuildCandidatesParams) ([]db.ListQueuedDeploymentBuildCandidatesRow, error)
	ListQueuedDeploymentBuildRegions(context.Context, int32) ([]string, error)
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
		QueueClass: "build", QueueName: "deployment-build", BuildAttemptNumber: row.BuildAttemptNumber,
		LeaseSequence: row.LeaseSequence, QueueTimestamp: row.QueueTimestamp.Time, EnqueuedAt: time.Now().UTC(),
		BuildResources: BuildResourceVector{CPUMillis: row.BuildRequestedCpuMillis, MemoryBytes: row.BuildRequestedMemoryBytes,
			WorkloadDiskBytes: row.BuildRequestedWorkloadDiskBytes, ScratchBytes: row.BuildRequestedScratchBytes,
			BuildCacheBytes: row.BuildRequestedBuildCacheBytes, ArtifactCacheBytes: row.BuildRequestedArtifactCacheBytes,
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
	row, err := e.store.PrepareQueuedRunDispatch(ctx, db.PrepareQueuedRunDispatchParams{
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
		OrgID:         scope.OrgID,
		RegionID:      scope.RegionID,
		ProjectID:     scope.ProjectID,
		EnvironmentID: scope.EnvironmentID,
		QueueClass:    scope.QueueClass,
		QueueName:     scope.QueueName,
		RowLimit:      limit,
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

func queueMessage(row db.PrepareQueuedRunDispatchRow) (Message, error) {
	requirements, err := requirementsFromRow(row)
	if err != nil {
		return Message{}, err
	}
	runID, err := pgUUIDString(row.ID)
	if err != nil {
		return Message{}, fmt.Errorf("run id: %w", err)
	}
	orgID, err := pgUUIDString(row.OrgID)
	if err != nil {
		return Message{}, fmt.Errorf("org id: %w", err)
	}
	projectID, err := pgUUIDString(row.ProjectID)
	if err != nil {
		return Message{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := pgUUIDString(row.EnvironmentID)
	if err != nil {
		return Message{}, fmt.Errorf("environment id: %w", err)
	}
	limit := int32(0)
	if row.QueueConcurrencyLimit.Valid {
		limit = row.QueueConcurrencyLimit.Int32
	}
	return Message{
		WorkKind:              WorkKindRun,
		RunID:                 runID,
		OrgID:                 orgID,
		RegionID:              row.RegionID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		QueueClass:            row.QueueClass,
		QueueName:             QueueNameForRuntime(row.QueueName, requirements.Runtime),
		QueueConcurrencyScope: row.QueueName,
		QueueConcurrencyLimit: limit,
		ConcurrencyKey:        row.ConcurrencyKey.String,
		RunStateVersion:       row.StateVersion,
		Requirements:          requirements,
		Priority:              row.Priority,
		QueueTimestamp:        row.QueueTimestamp.Time,
		QueuedExpiresAt:       row.QueuedExpiresAt.Time,
		EnqueuedAt:            time.Now().UTC(),
	}, nil
}

func requirementsFromRow(row db.PrepareQueuedRunDispatchRow) (compute.RunRuntimeRequirements, error) {
	return compute.RunRuntimeRequirementsFromFields(compute.RunRuntimeRequirementFields{
		RequestedMilliCPU:       row.RequestedMilliCpu,
		RequestedMemoryMiB:      row.RequestedMemoryMib,
		RequestedDiskMiB:        row.RequestedDiskMib,
		RequestedExecutionSlots: row.RequestedExecutionSlots,
		RuntimeID:               row.RuntimeIdentityID,
		RuntimeArch:             row.RuntimeArch,
		RuntimeABI:              row.RuntimeABI,
		KernelDigest:            row.KernelDigest,
		InitramfsDigest:         row.InitramfsDigest,
		RootfsDigest:            row.RootfsDigest,
		CNIProfile:              row.CniProfile,
		NetworkPolicyJSON:       row.NetworkPolicy,
		PlacementJSON:           row.ResourcePlacementPolicy,
	})
}

func pgUUIDString(value pgtype.UUID) (string, error) {
	parsed, err := pgvalue.UUIDValue(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}
