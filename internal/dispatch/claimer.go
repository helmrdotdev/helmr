package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var ErrNoClaim = errors.New("no queue lease available")

var errInvalidLease = errors.New("invalid queue lease")

const DefaultMaxDispatchAttempts int32 = 5

type ClaimerStore interface {
	DeadLetterRunQueueItem(context.Context, db.DeadLetterRunQueueItemParams) (db.DeadLetterRunQueueItemRow, error)
	IsRunQueueLeaseConflict(context.Context, db.IsRunQueueLeaseConflictParams) (bool, error)
	ReserveRunQueueItem(context.Context, db.ReserveRunQueueItemParams) (db.RunQueueItem, error)
	RunLeaseDispatchAttemptsExhausted(context.Context, db.RunLeaseDispatchAttemptsExhaustedParams) (bool, error)
}

type Claimer struct {
	store               ClaimerStore
	queue               Queue
	maxDispatchAttempts int32
}

type ClaimerOption func(*Claimer)

func WithMaxDispatchAttempts(maxAttempts int32) ClaimerOption {
	return func(c *Claimer) {
		c.maxDispatchAttempts = maxAttempts
	}
}

func NewClaimer(store ClaimerStore, queue Queue, opts ...ClaimerOption) (*Claimer, error) {
	if store == nil {
		return nil, errors.New("queue store is required")
	}
	if queue == nil {
		return nil, errors.New("queue is required")
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

type ClaimRequest struct {
	DequeueRequest
}

type ClaimedRun struct {
	Lease Lease
	Entry db.RunQueueItem
}

func (c *Claimer) Claim(ctx context.Context, request ClaimRequest) (ClaimedRun, error) {
	if strings.TrimSpace(request.WorkerInstanceID) == "" {
		return ClaimedRun{}, errors.New("worker instance id is required")
	}
	leases, err := c.queue.Dequeue(ctx, request.DequeueRequest)
	if err != nil {
		return ClaimedRun{}, err
	}
	for _, lease := range leases {
		if strings.TrimSpace(lease.MessageID) == "" {
			_ = c.queue.Nack(ctx, lease, NackReasonInvalid)
			continue
		}
		exhausted, err := c.deliveryAttemptsExhausted(ctx, lease)
		if errors.Is(err, errInvalidLease) {
			_ = c.queue.Nack(ctx, lease, NackReasonInvalid)
			continue
		}
		if err != nil {
			return ClaimedRun{}, err
		}
		if exhausted {
			err := c.deadLetter(ctx, lease)
			if ackErr := c.queue.Ack(ctx, lease); ackErr != nil {
				err = errors.Join(err, ackErr)
			}
			if err != nil {
				return ClaimedRun{}, err
			}
			continue
		}
		leased, err := c.markLeased(ctx, lease)
		if err == nil {
			return ClaimedRun{Lease: lease, Entry: leased}, nil
		}
		reason := NackReasonInvalid
		suppressClaimErr := errors.Is(err, pgx.ErrNoRows) || errors.Is(err, errInvalidLease)
		switch {
		case errors.Is(err, errInvalidLease):
			reason = NackReasonInvalid
		case errors.Is(err, pgx.ErrNoRows):
			conflict, conflictErr := c.isLeaseConflict(ctx, lease)
			if conflictErr != nil {
				err = errors.Join(err, conflictErr)
				reason = NackReasonRetry
				suppressClaimErr = false
			} else if conflict {
				reason = NackReasonLeaseConflict
			}
		default:
			reason = NackReasonRetry
			suppressClaimErr = false
		}
		nackErr := c.queue.Nack(ctx, lease, reason)
		if nackErr != nil {
			err = errors.Join(err, nackErr)
			suppressClaimErr = false
		}
		if !suppressClaimErr {
			return ClaimedRun{}, err
		}
	}
	return ClaimedRun{}, ErrNoClaim
}

func (c *Claimer) deliveryAttemptsExhausted(ctx context.Context, lease Lease) (bool, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return false, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return false, err
	}
	return c.store.RunLeaseDispatchAttemptsExhausted(ctx, db.RunLeaseDispatchAttemptsExhaustedParams{
		OrgID:               orgID,
		RunID:               runID,
		MaxDispatchAttempts: c.maxDispatchAttempts,
	})
}

func (c *Claimer) deadLetter(ctx context.Context, lease Lease) error {
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

func (c *Claimer) markLeased(ctx context.Context, lease Lease) (db.RunQueueItem, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	workerInstanceID, err := parseUUID("worker instance id", lease.WorkerInstanceID)
	if err != nil {
		return db.RunQueueItem{}, err
	}
	return c.store.ReserveRunQueueItem(ctx, db.ReserveRunQueueItemParams{
		OrgID:                orgID,
		RunID:                runID,
		WorkerInstanceID:     workerInstanceID,
		DispatchMessageID:    pgtype.Text{String: lease.MessageID, Valid: true},
		ReservationExpiresAt: pgtype.Timestamptz{Time: lease.ExpiresAt, Valid: true},
	})
}

func (c *Claimer) isLeaseConflict(ctx context.Context, lease Lease) (bool, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return false, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return false, err
	}
	return c.store.IsRunQueueLeaseConflict(ctx, db.IsRunQueueLeaseConflictParams{
		OrgID:             orgID,
		RunID:             runID,
		DispatchMessageID: pgtype.Text{String: lease.MessageID, Valid: true},
	})
}

func parseUUID(label string, value string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %s: %v", errInvalidLease, label, err)
	}
	return pgvalue.UUID(parsed), nil
}
