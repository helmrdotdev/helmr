package dispatch

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNoEnqueueCandidate = errors.New("no queue candidate")

type EnqueuerStore interface {
	PrepareQueuedRunQueueItem(context.Context, db.PrepareQueuedRunQueueItemParams) (db.PrepareQueuedRunQueueItemRow, error)
	ListQueuedRunQueueItemCandidatesForScope(context.Context, db.ListQueuedRunQueueItemCandidatesForScopeParams) ([]db.ListQueuedRunQueueItemCandidatesForScopeRow, error)
	MarkRunQueueItemEnqueued(context.Context, db.MarkRunQueueItemEnqueuedParams) (db.RunQueueItem, error)
	MarkRunQueueItemEnqueueError(context.Context, db.MarkRunQueueItemEnqueueErrorParams) (db.RunQueueItem, error)
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
	row, err := e.store.PrepareQueuedRunQueueItem(ctx, db.PrepareQueuedRunQueueItemParams{
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
		_, markErr := e.store.MarkRunQueueItemEnqueueError(ctx, db.MarkRunQueueItemEnqueueErrorParams{
			OrgID:                      orgID,
			RunID:                      runID,
			WorkerGroupID:              row.WorkerGroupID,
			QueueClass:                 row.QueueClass,
			LastError:                  truncateError(err, e.errorSize),
			ExpectedDispatchGeneration: row.DispatchGeneration,
		})
		return EnqueueResult{}, errors.Join(err, markErr)
	}
	if _, err := e.store.MarkRunQueueItemEnqueued(ctx, db.MarkRunQueueItemEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		WorkerGroupID:              row.WorkerGroupID,
		QueueClass:                 row.QueueClass,
		DispatchMessageID:          pgtype.Text{String: result.MessageID, Valid: true},
		ExpectedDispatchGeneration: row.DispatchGeneration,
	}); err != nil {
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
	candidates, err := e.store.ListQueuedRunQueueItemCandidatesForScope(ctx, db.ListQueuedRunQueueItemCandidatesForScopeParams{
		OrgID:         scope.OrgID,
		WorkerGroupID: scope.WorkerGroupID,
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
		if candidate.DispatchMessageID != "" {
			exists, err := e.queue.ReadyMessageExists(ctx, candidate.DispatchMessageID)
			if err != nil {
				stats.Failed++
				problems = append(problems, err)
				continue
			}
			if exists {
				stats.Skipped++
				continue
			}
		}
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

func queueMessage(row db.PrepareQueuedRunQueueItemRow) (Message, error) {
	requirements, err := requirementsFromRow(row)
	if err != nil {
		return Message{}, err
	}
	runID, err := pgUUIDString(row.RunID)
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
		RunID:                 runID,
		OrgID:                 orgID,
		WorkerGroupID:         row.WorkerGroupID,
		ProjectID:             projectID,
		EnvironmentID:         environmentID,
		QueueClass:            row.QueueClass,
		QueueName:             QueueNameForRuntime(row.QueueName, requirements.Runtime),
		QueueConcurrencyScope: row.QueueName,
		QueueConcurrencyLimit: limit,
		ConcurrencyKey:        row.ConcurrencyKey.String,
		Requirements:          requirements,
		Priority:              row.Priority,
		QueueTimestamp:        row.QueueTimestamp.Time,
		QueuedExpiresAt:       row.QueuedExpiresAt.Time,
		EnqueuedAt:            row.EnqueuedAt.Time,
	}, nil
}

func requirementsFromRow(row db.PrepareQueuedRunQueueItemRow) (compute.RunRuntimeRequirements, error) {
	return compute.RunRuntimeRequirementsFromFields(compute.RunRuntimeRequirementFields{
		RequestedMilliCPU:       row.RequestedMilliCpu,
		RequestedMemoryMiB:      row.RequestedMemoryMib,
		RequestedDiskMiB:        row.RequestedDiskMib,
		RequestedExecutionSlots: row.RequestedExecutionSlots,
		RuntimeID:               row.RuntimeID,
		RuntimeArch:             row.RuntimeArch,
		RuntimeABI:              row.RuntimeABI,
		KernelDigest:            row.KernelDigest,
		InitramfsDigest:         row.InitramfsDigest,
		RootfsDigest:            row.RootfsDigest,
		CNIProfile:              row.CniProfile,
		NetworkPolicyJSON:       row.NetworkPolicy,
		PlacementJSON:           row.Placement,
	})
}

func pgUUIDString(value pgtype.UUID) (string, error) {
	parsed, err := pgvalue.UUIDValue(value)
	if err != nil {
		return "", err
	}
	return parsed.String(), nil
}

func truncateError(err error, limit int) string {
	if err == nil {
		return ""
	}
	text := err.Error()
	if limit <= 0 {
		return ""
	}
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.ValidString(text[:limit]) {
		limit--
	}
	return text[:limit]
}
