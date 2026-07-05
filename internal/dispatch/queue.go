package dispatch

import (
	"context"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type DequeueRequest struct {
	OrgID            string
	CellID           string
	ProjectID        string
	EnvironmentID    string
	QueueClass       string
	WorkerInstanceID string
	QueueName        string
	Region           string
	Available        compute.ResourceVector
	Runtime          compute.RuntimeSelector
	Labels           map[string]string
	MaxMessages      int
	Wait             time.Duration
}

type EnqueueResult struct {
	QueueName string
	MessageID string
	Depth     int64
}

type Queue interface {
	Enqueue(context.Context, Message) (EnqueueResult, error)
	Dequeue(context.Context, DequeueRequest) ([]Lease, error)
	ReadyMessageExists(context.Context, string) (bool, error)
	Ack(context.Context, Lease) error
	Nack(context.Context, Lease, NackReason) error
	Renew(context.Context, Lease, time.Time) (Lease, error)
}

type NackReason string

const (
	NackReasonRetry         NackReason = "retry"
	NackReasonNoCapacity    NackReason = "no_capacity"
	NackReasonInvalid       NackReason = "invalid"
	NackReasonHostDraining  NackReason = "host_draining"
	NackReasonLeaseConflict NackReason = "lease_conflict"
)
