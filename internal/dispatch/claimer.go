package dispatch

import (
	"context"
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
	DeadLetterRunDispatch(context.Context, db.DeadLetterRunDispatchParams) (db.DeadLetterRunDispatchRow, error)
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
	Entry db.Run
}

func (c *Claimer) Claim(ctx context.Context, request ClaimRequest) (ClaimedRun, error) {
	if strings.TrimSpace(request.WorkerInstanceID) == "" {
		return ClaimedRun{}, errors.New("worker instance id is required")
	}
	leases, err := c.queue.Dequeue(ctx, request.DequeueRequest)
	if err != nil {
		return ClaimedRun{}, err
	}
	var cleanupErr error
	for _, lease := range leases {
		if strings.TrimSpace(lease.MessageID) == "" {
			_ = c.queue.Nack(ctx, lease, NackReasonInvalid)
			continue
		}
		exhausted, err := c.deliveryAttemptsExhausted(ctx, lease)
		if errors.Is(err, errInvalidLease) || errors.Is(err, pgx.ErrNoRows) {
			_ = c.queue.Nack(ctx, lease, NackReasonInvalid)
			continue
		}
		if err != nil {
			return ClaimedRun{}, err
		}
		if exhausted {
			err := c.deadLetter(ctx, lease)
			if errors.Is(err, errInvalidLease) || errors.Is(err, pgx.ErrNoRows) {
				_ = c.queue.Nack(ctx, lease, NackReasonInvalid)
				continue
			}
			if err != nil {
				return ClaimedRun{}, err
			}
			if ackErr := c.queue.Ack(ctx, lease); ackErr != nil {
				cleanupErr = errors.Join(cleanupErr, ackErr)
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
			reason = NackReasonInvalid
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
	if cleanupErr != nil {
		return ClaimedRun{}, cleanupErr
	}
	return ClaimedRun{}, ErrNoClaim
}

func (c *Claimer) deliveryAttemptsExhausted(ctx context.Context, lease Lease) (bool, error) {
	scope, err := runQueueLeaseScope(lease)
	if err != nil {
		return false, err
	}
	return c.store.RunLeaseDispatchAttemptsExhausted(ctx, db.RunLeaseDispatchAttemptsExhaustedParams{
		OrgID:               scope.orgID,
		WorkerGroupID:       scope.workerGroupID,
		QueueClass:          scope.queueClass,
		RunID:               scope.runID,
		DispatchGeneration:  lease.Message.DispatchGeneration,
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
	workerGroupID := strings.TrimSpace(lease.Message.WorkerGroupID)
	if workerGroupID == "" {
		return fmt.Errorf("%w: worker group id is required", errInvalidLease)
	}
	queueClass := strings.TrimSpace(lease.Message.QueueClass)
	if queueClass == "" {
		return fmt.Errorf("%w: queue class is required", errInvalidLease)
	}
	lastError := fmt.Sprintf("run exceeded max dispatch attempts (%d)", c.maxDispatchAttempts)
	_, err = c.store.DeadLetterRunDispatch(ctx, db.DeadLetterRunDispatchParams{
		OrgID:              orgID,
		WorkerGroupID:      workerGroupID,
		QueueClass:         queueClass,
		RunID:              runID,
		DispatchGeneration: lease.Message.DispatchGeneration,
		LastError:          lastError,
	})
	return err
}

func (c *Claimer) markLeased(ctx context.Context, lease Lease) (db.Run, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return db.Run{}, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return db.Run{}, err
	}
	workerGroupID := strings.TrimSpace(lease.Message.WorkerGroupID)
	if workerGroupID == "" {
		return db.Run{}, fmt.Errorf("%w: worker group id is required", errInvalidLease)
	}
	queueClass := strings.TrimSpace(lease.Message.QueueClass)
	if queueClass == "" {
		return db.Run{}, fmt.Errorf("%w: queue class is required", errInvalidLease)
	}
	if lease.Message.DispatchGeneration <= 0 {
		return db.Run{}, fmt.Errorf("%w: dispatch generation is required", errInvalidLease)
	}
	return db.Run{
		OrgID:              orgID,
		WorkerGroupID:      workerGroupID,
		ID:                 runID,
		QueueClass:         queueClass,
		QueueName:          strings.TrimSpace(lease.Message.QueueName),
		Priority:           lease.Message.Priority,
		QueueTimestamp:     pgvalue.Timestamptz(lease.Message.QueueTimestamp),
		QueuedExpiresAt:    pgvalue.TimestamptzUTCZeroInvalid(lease.Message.QueuedExpiresAt),
		DispatchGeneration: lease.Message.DispatchGeneration,
		Status:             db.RunStatusQueued,
	}, nil
}

type runQueueScope struct {
	orgID         pgtype.UUID
	workerGroupID string
	queueClass    string
	runID         pgtype.UUID
}

func runQueueLeaseScope(lease Lease) (runQueueScope, error) {
	orgID, err := parseUUID("org id", lease.Message.OrgID)
	if err != nil {
		return runQueueScope{}, err
	}
	runID, err := parseUUID("run id", lease.Message.RunID)
	if err != nil {
		return runQueueScope{}, err
	}
	workerGroupID := strings.TrimSpace(lease.Message.WorkerGroupID)
	if workerGroupID == "" {
		return runQueueScope{}, fmt.Errorf("%w: worker group id is required", errInvalidLease)
	}
	queueClass := strings.TrimSpace(lease.Message.QueueClass)
	if queueClass == "" {
		return runQueueScope{}, fmt.Errorf("%w: queue class is required", errInvalidLease)
	}
	return runQueueScope{
		orgID:         orgID,
		workerGroupID: workerGroupID,
		queueClass:    queueClass,
		runID:         runID,
	}, nil
}

func parseUUID(label string, value string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %s: %v", errInvalidLease, label, err)
	}
	return pgvalue.UUID(parsed), nil
}
