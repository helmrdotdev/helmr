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

type Claimer struct {
	queue Queue
}

func NewClaimer(queue Queue) (*Claimer, error) {
	if queue == nil {
		return nil, errors.New("queue is required")
	}
	return &Claimer{queue: queue}, nil
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
		leased, err := c.markLeased(lease)
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

func (c *Claimer) markLeased(lease Lease) (db.Run, error) {
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

func parseUUID(label string, value string) (pgtype.UUID, error) {
	parsed, err := uuid.Parse(strings.TrimSpace(value))
	if err != nil {
		return pgtype.UUID{}, fmt.Errorf("%w: %s: %v", errInvalidLease, label, err)
	}
	return pgvalue.UUID(parsed), nil
}
