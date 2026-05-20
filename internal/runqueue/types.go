package runqueue

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type QueueMessage struct {
	RunID         string
	OrgID         string
	ProjectID     string
	EnvironmentID string
	WorkerGroupID string
	QueueName     string
	Requirements  compute.RunRequirements
	Priority      int32
	EnqueuedAt    time.Time
	Traceparent   string
}

func (m QueueMessage) Validate() error {
	var problems []error
	if strings.TrimSpace(m.RunID) == "" {
		problems = append(problems, errors.New("run id is required"))
	}
	if strings.TrimSpace(m.OrgID) == "" {
		problems = append(problems, errors.New("org id is required"))
	}
	if strings.TrimSpace(m.WorkerGroupID) == "" {
		problems = append(problems, errors.New("worker group id is required"))
	}
	if strings.TrimSpace(m.QueueName) == "" {
		problems = append(problems, errors.New("queue name is required"))
	}
	if err := m.Requirements.Validate(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

type Lease struct {
	ID            string
	MessageID     string
	Message       QueueMessage
	WorkerHostID  string
	ExecutionID   string
	AttemptNumber int32
	ExpiresAt     time.Time
}

type DequeueRequest struct {
	OrgID         string
	WorkerGroupID string
	WorkerHostID  string
	QueueName     string
	Available     compute.ResourceVector
	Runtime       compute.RuntimeSelector
	Region        string
	Labels        map[string]string
	MaxMessages   int
	Wait          time.Duration
}

type EnqueueResult struct {
	QueueName string
	MessageID string
	Depth     int64
}

type RunQueue interface {
	Enqueue(context.Context, QueueMessage) (EnqueueResult, error)
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
