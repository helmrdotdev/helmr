package dispatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
)

type Message struct {
	RunID                 string
	OrgID                 string
	ProjectID             string
	EnvironmentID         string
	QueueName             string
	QueueConcurrencyScope string
	QueueConcurrencyLimit int32
	ConcurrencyKey        string
	Requirements          compute.RunRuntimeRequirements
	Priority              int32
	QueueTimestamp        time.Time
	QueuedExpiresAt       time.Time
	EnqueuedAt            time.Time
	Traceparent           string
}

func QueueNameForRuntime(base string, runtime compute.RuntimeSelector) string {
	names := QueueNamesForRuntime(base, runtime)
	if len(names) == 0 {
		return strings.TrimSpace(base)
	}
	return names[0]
}

func QueueNamesForRuntime(base string, runtime compute.RuntimeSelector) []string {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil
	}
	parts := runtimeQueueParts(runtime)
	names := make([]string, 0, len(parts)+1)
	for i := len(parts); i > 0; i-- {
		names = append(names, base+":rt:"+strings.Join(parts[:i], ":"))
	}
	names = append(names, base)
	return names
}

func runtimeQueueParts(runtime compute.RuntimeSelector) []string {
	ordered := []string{
		strings.TrimSpace(runtime.Arch),
		strings.TrimSpace(runtime.ABI),
		strings.TrimSpace(runtime.KernelDigest),
		strings.TrimSpace(runtime.RootfsDigest),
		strings.TrimSpace(runtime.CNIProfile),
	}
	parts := make([]string, 0, len(ordered))
	for _, part := range ordered {
		if part == "" {
			break
		}
		parts = append(parts, part)
	}
	return parts
}

func (m Message) Validate() error {
	var problems []error
	if strings.TrimSpace(m.RunID) == "" {
		problems = append(problems, errors.New("run id is required"))
	}
	if strings.TrimSpace(m.OrgID) == "" {
		problems = append(problems, errors.New("org id is required"))
	}
	if strings.TrimSpace(m.QueueName) == "" {
		problems = append(problems, errors.New("queue name is required"))
	}
	if m.QueueConcurrencyLimit < 0 {
		problems = append(problems, errors.New("queue concurrency limit must be non-negative"))
	}
	if m.QueueConcurrencyLimit > 0 {
		if strings.TrimSpace(m.ProjectID) == "" {
			problems = append(problems, errors.New("project id is required when queue concurrency is limited"))
		}
		if strings.TrimSpace(m.EnvironmentID) == "" {
			problems = append(problems, errors.New("environment id is required when queue concurrency is limited"))
		}
	}
	if err := m.Requirements.Validate(); err != nil {
		problems = append(problems, err)
	}
	return errors.Join(problems...)
}

type Lease struct {
	ID               string
	MessageID        string
	Message          Message
	WorkerInstanceID string
	ExecutionID      string
	AttemptNumber    int32
	ExpiresAt        time.Time
}

type DequeueRequest struct {
	OrgID            string
	WorkerInstanceID string
	QueueName        string
	Available        compute.ResourceVector
	Runtime          compute.RuntimeSelector
	Region           string
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
