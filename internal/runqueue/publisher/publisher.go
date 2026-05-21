package publisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNoQueueCandidate = errors.New("no queue candidate")

type Store interface {
	PrepareQueuedRunQueueEntry(context.Context, db.PrepareQueuedRunQueueEntryParams) (db.PrepareQueuedRunQueueEntryRow, error)
	ListQueuedRunQueueEntryCandidates(context.Context, db.ListQueuedRunQueueEntryCandidatesParams) ([]db.ListQueuedRunQueueEntryCandidatesRow, error)
	MarkRunQueueEntryEnqueued(context.Context, db.MarkRunQueueEntryEnqueuedParams) (db.RunQueueEntry, error)
	MarkRunQueueEntryEnqueueError(context.Context, db.MarkRunQueueEntryEnqueueErrorParams) (db.RunQueueEntry, error)
}

type Publisher struct {
	store     Store
	queue     runqueue.Queue
	priority  int32
	errorSize int
}

type Option func(*Publisher)

func New(store Store, queue runqueue.Queue, opts ...Option) (*Publisher, error) {
	if store == nil {
		return nil, errors.New("queue store is required")
	}
	if queue == nil {
		return nil, errors.New("run queue is required")
	}
	e := &Publisher{
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

func WithPriority(priority int32) Option {
	return func(e *Publisher) {
		e.priority = priority
	}
}

func (e *Publisher) EnqueueRun(ctx context.Context, orgID pgtype.UUID, runID pgtype.UUID) (runqueue.EnqueueResult, error) {
	row, err := e.store.PrepareQueuedRunQueueEntry(ctx, db.PrepareQueuedRunQueueEntryParams{
		OrgID:    orgID,
		RunID:    runID,
		Priority: e.priority,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return runqueue.EnqueueResult{}, ErrNoQueueCandidate
	}
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	message, err := queueMessage(row)
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	result, err := e.queue.Enqueue(ctx, message)
	if err != nil {
		_, markErr := e.store.MarkRunQueueEntryEnqueueError(ctx, db.MarkRunQueueEntryEnqueueErrorParams{
			OrgID:                      orgID,
			RunID:                      runID,
			LastError:                  truncateError(err, e.errorSize),
			ExpectedDispatchGeneration: row.DispatchGeneration,
		})
		return runqueue.EnqueueResult{}, errors.Join(err, markErr)
	}
	if _, err := e.store.MarkRunQueueEntryEnqueued(ctx, db.MarkRunQueueEntryEnqueuedParams{
		OrgID:                      orgID,
		RunID:                      runID,
		QueueMessageID:             pgtype.Text{String: result.MessageID, Valid: true},
		ExpectedDispatchGeneration: row.DispatchGeneration,
	}); err != nil {
		return runqueue.EnqueueResult{}, err
	}
	return result, nil
}

type ReconcileStats struct {
	Scanned  int
	Enqueued int
	Skipped  int
	Failed   int
}

func (e *Publisher) ReconcileOrg(ctx context.Context, orgID pgtype.UUID, limit int32) (ReconcileStats, error) {
	if limit <= 0 {
		limit = 100
	}
	candidates, err := e.store.ListQueuedRunQueueEntryCandidates(ctx, db.ListQueuedRunQueueEntryCandidatesParams{
		OrgID:    orgID,
		RowLimit: limit,
	})
	if err != nil {
		return ReconcileStats{}, err
	}
	stats := ReconcileStats{Scanned: len(candidates)}
	var problems []error
	for _, candidate := range candidates {
		if candidate.QueueMessageID != "" {
			exists, err := e.queue.ReadyMessageExists(ctx, candidate.QueueMessageID)
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
			if errors.Is(err, ErrNoQueueCandidate) {
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

func queueMessage(row db.PrepareQueuedRunQueueEntryRow) (runqueue.Message, error) {
	requirements, err := requirementsFromRow(row)
	if err != nil {
		return runqueue.Message{}, err
	}
	runID, err := pgUUIDString(row.RunID)
	if err != nil {
		return runqueue.Message{}, fmt.Errorf("run id: %w", err)
	}
	orgID, err := pgUUIDString(row.OrgID)
	if err != nil {
		return runqueue.Message{}, fmt.Errorf("org id: %w", err)
	}
	projectID, err := pgUUIDString(row.ProjectID)
	if err != nil {
		return runqueue.Message{}, fmt.Errorf("project id: %w", err)
	}
	environmentID, err := pgUUIDString(row.EnvironmentID)
	if err != nil {
		return runqueue.Message{}, fmt.Errorf("environment id: %w", err)
	}
	workerPoolID, err := pgUUIDString(row.WorkerPoolID)
	if err != nil {
		return runqueue.Message{}, fmt.Errorf("worker pool id: %w", err)
	}
	return runqueue.Message{
		RunID:         runID,
		OrgID:         orgID,
		ProjectID:     projectID,
		EnvironmentID: environmentID,
		WorkerPoolID:  workerPoolID,
		QueueName:     runqueue.QueueNameForRuntime(row.QueueName, requirements.Runtime),
		Requirements:  requirements,
		Priority:      row.Priority,
		EnqueuedAt:    row.EnqueuedAt.Time,
	}, nil
}

func requirementsFromRow(row db.PrepareQueuedRunQueueEntryRow) (compute.RunRequirements, error) {
	var network compute.NetworkPolicy
	if len(row.NetworkPolicy) > 0 {
		if err := json.Unmarshal(row.NetworkPolicy, &network); err != nil {
			return compute.RunRequirements{}, fmt.Errorf("network policy: %w", err)
		}
	}
	var placement compute.Placement
	if len(row.Placement) > 0 {
		if err := json.Unmarshal(row.Placement, &placement); err != nil {
			return compute.RunRequirements{}, fmt.Errorf("placement: %w", err)
		}
	}
	requirements := compute.RunRequirements{
		Resources: compute.ResourceVector{
			MilliCPU:  row.RequestedMilliCpu,
			MemoryMiB: row.RequestedMemoryMib,
			DiskMiB:   row.RequestedDiskMib,
			Slots:     row.RequestedExecutionSlots,
		},
		Runtime: compute.RuntimeSelector{
			Arch:         row.RuntimeArch,
			ABI:          row.RuntimeABI,
			KernelDigest: row.KernelDigest,
			RootfsDigest: row.RootfsDigest,
			CNIProfile:   row.CniProfile,
		},
		Network:   network,
		Placement: placement,
	}
	if err := requirements.Validate(); err != nil {
		return compute.RunRequirements{}, err
	}
	return requirements, nil
}

func pgUUIDString(value pgtype.UUID) (string, error) {
	parsed, err := ids.FromPG(value)
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
	if len(text) <= limit {
		return text
	}
	return text[:limit]
}
