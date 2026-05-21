package claimer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNoLease = errors.New("no queue lease available")

var errInvalidLease = errors.New("invalid queue lease")

const DefaultMaxDispatchAttempts int32 = 5

type Store interface {
	DeadLetterRunQueueItem(context.Context, db.DeadLetterRunQueueItemParams) (db.DeadLetterRunQueueItemRow, error)
	ReserveRunQueueItem(context.Context, db.ReserveRunQueueItemParams) (db.RunQueueItem, error)
	RunExecutionDispatchAttemptsExhausted(context.Context, db.RunExecutionDispatchAttemptsExhaustedParams) (bool, error)
}

type Claimer struct {
	store               Store
	queue               runqueue.Queue
	maxDispatchAttempts int32
}

type Option func(*Claimer)

func WithMaxDispatchAttempts(maxAttempts int32) Option {
	return func(c *Claimer) {
		c.maxDispatchAttempts = maxAttempts
	}
}

func New(store Store, queue runqueue.Queue, opts ...Option) (*Claimer, error) {
	if store == nil {
		return nil, errors.New("queue store is required")
	}
	if queue == nil {
		return nil, errors.New("run queue is required")
	}
	claimer := &Claimer{
		store:               store,
		queue:               queue,
		maxDispatchAttempts: DefaultMaxDispatchAttempts,
	}
	for _, opt := range opts {
		opt(claimer)
	}
	if claimer.maxDispatchAttempts <= 0 {
		return nil, errors.New("max dispatch attempts must be positive")
	}
	return claimer, nil
}

type LeaseRequest struct {
	runqueue.DequeueRequest
}

type Result struct {
	Lease runqueue.Lease
	Entry db.RunQueueItem
}

func (c *Claimer) Lease(ctx context.Context, request LeaseRequest) (Result, error) {
	if strings.TrimSpace(request.WorkerInstanceID) == "" {
		return Result{}, errors.New("worker instance id is required")
	}
	leases, err := c.queue.Dequeue(ctx, request.DequeueRequest)
	if err != nil {
		return Result{}, err
	}
	for _, lease := range leases {
		if strings.TrimSpace(lease.MessageID) == "" {
			_ = c.queue.Nack(ctx, lease, runqueue.NackReasonInvalid)
			continue
		}
		exhausted, err := c.deliveryAttemptsExhausted(ctx, lease)
		if errors.Is(err, errInvalidLease) {
			_ = c.queue.Nack(ctx, lease, runqueue.NackReasonInvalid)
			continue
		}
		if err != nil {
			return Result{}, err
		}
		if exhausted {
			err := c.deadLetter(ctx, lease)
			if ackErr := c.queue.Ack(ctx, lease); ackErr != nil {
				err = errors.Join(err, ackErr)
			}
			if err != nil {
				return Result{}, err
			}
			continue
		}
		leased, err := c.markLeased(ctx, lease)
		if err == nil {
			return Result{Lease: lease, Entry: leased}, nil
		}
		reason := runqueue.NackReasonLeaseConflict
		switch {
		case errors.Is(err, errInvalidLease):
			reason = runqueue.NackReasonInvalid
		case errors.Is(err, pgx.ErrNoRows):
			reason = runqueue.NackReasonInvalid
		case !errors.Is(err, pgx.ErrNoRows):
			reason = runqueue.NackReasonRetry
		}
		nackErr := c.queue.Nack(ctx, lease, reason)
		if nackErr != nil {
			err = errors.Join(err, nackErr)
		}
		if !errors.Is(err, pgx.ErrNoRows) && !errors.Is(err, errInvalidLease) {
			return Result{}, err
		}
	}
	return Result{}, ErrNoLease
}

func (c *Claimer) deliveryAttemptsExhausted(ctx context.Context, lease runqueue.Lease) (bool, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return false, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return false, err
	}
	return c.store.RunExecutionDispatchAttemptsExhausted(ctx, db.RunExecutionDispatchAttemptsExhaustedParams{
		OrgID:               orgID,
		RunID:               runID,
		MaxDispatchAttempts: c.maxDispatchAttempts,
	})
}

func (c *Claimer) deadLetter(ctx context.Context, lease runqueue.Lease) error {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return err
	}
	lastError := fmt.Sprintf("run exceeded max dispatch attempts (%d)", c.maxDispatchAttempts)
	payload, err := json.Marshal(map[string]any{
		"reason":                 "max_dispatch_attempts_exceeded",
		"queue_dispatch_attempt": lease.AttemptNumber,
		"max_dispatch_attempts":  c.maxDispatchAttempts,
	})
	if err != nil {
		return err
	}
	_, err = c.store.DeadLetterRunQueueItem(ctx, db.DeadLetterRunQueueItemParams{
		OrgID:             orgID,
		RunID:             runID,
		DispatchMessageID: pgtype.Text{String: lease.MessageID, Valid: true},
		LastError:         lastError,
		EventKind:         "run.dead_lettered",
		EventPayload:      payload,
	})
	return err
}

func (c *Claimer) markLeased(ctx context.Context, lease runqueue.Lease) (db.RunQueueItem, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	workerHostID, err := parseUUID("worker instance id", lease.WorkerInstanceID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	return c.store.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     workerHostID,
		DispatchMessageID:    pgtype.Text{String: lease.MessageID, Valid: true},
		ReservationExpiresAt: pgtype.Timestamptz{Time: lease.ExpiresAt, Valid: true},
	})
}

func parseUUID(label string, value string) (pgtype.UUID, error) {
	parsed, err := ids.Parse(strings.TrimSpace(value))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %s: %v", errInvalidLease, label, err)
	}
	return ids.ToPG(parsed), nil
}
