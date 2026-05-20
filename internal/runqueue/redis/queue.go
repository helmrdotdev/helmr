package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/runqueue"
	goredis "github.com/redis/go-redis/v9"
)

const (
	defaultPrefix              = "helmr:dispatch"
	defaultLease               = 5 * time.Minute
	defaultGenerationSafetyTTL = 30 * 24 * time.Hour
	defaultMaxMessages         = 1
	defaultReclaim             = 128
	defaultScanLimit           = 128
)

type Queue struct {
	client        goredis.Cmdable
	prefix        string
	leaseTimeout  time.Duration
	generationTTL time.Duration
	now           func() time.Time
}

type Option func(*Queue)

func New(client goredis.Cmdable, opts ...Option) (*Queue, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}
	queue := &Queue{
		client:        client,
		prefix:        defaultPrefix,
		leaseTimeout:  defaultLease,
		generationTTL: defaultGenerationSafetyTTL,
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(queue)
	}
	if strings.TrimSpace(queue.prefix) == "" {
		return nil, errors.New("redis key prefix is required")
	}
	if queue.leaseTimeout <= 0 {
		return nil, errors.New("redis lease timeout must be positive")
	}
	if queue.generationTTL <= 0 {
		return nil, errors.New("redis generation safety ttl must be positive")
	}
	if queue.now == nil {
		return nil, errors.New("redis clock is required")
	}
	return queue, nil
}

func WithPrefix(prefix string) Option {
	return func(q *Queue) {
		q.prefix = strings.TrimRight(prefix, ":")
	}
}

func WithLeaseTimeout(timeout time.Duration) Option {
	return func(q *Queue) {
		q.leaseTimeout = timeout
	}
}

func WithGenerationSafetyTTL(ttl time.Duration) Option {
	return func(q *Queue) {
		q.generationTTL = ttl
	}
}

func WithClock(now func() time.Time) Option {
	return func(q *Queue) {
		q.now = now
	}
}

func (q *Queue) Enqueue(ctx context.Context, message runqueue.Message) (runqueue.EnqueueResult, error) {
	if message.EnqueuedAt.IsZero() {
		message.EnqueuedAt = q.now().UTC()
	}
	if err := message.Validate(); err != nil {
		return runqueue.EnqueueResult{}, err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	placementLabels, err := jsonMap(message.Requirements.Placement.Tags)
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	keys := q.keys(message.OrgID, message.WorkerGroupID, message.QueueName)
	score := readyScore(message.Priority, message.EnqueuedAt)
	resources := message.Requirements.Resources
	runtime := message.Requirements.Runtime
	placement := message.Requirements.Placement

	result, err := q.client.Eval(ctx, enqueueScript, []string{keys.ready},
		q.prefix,
		keys.scope,
		keys.orgRunScope,
		sanitizeKeyPart(message.RunID),
		payload,
		strconv.FormatFloat(score, 'f', -1, 64),
		resources.MilliCPU,
		resources.MemoryMiB,
		resources.DiskMiB,
		resources.Slots,
		runtime.Arch,
		runtime.ABI,
		runtime.KernelDigest,
		runtime.RootfsDigest,
		runtime.CNIProfile,
		placement.Region,
		placementLabels,
		placement.DedicatedKey,
		placement.SnapshotKey,
		q.generationTTL.Milliseconds(),
	).Result()
	if err != nil {
		return runqueue.EnqueueResult{}, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
	}
	fields, ok := result.([]interface{})
	if !ok || len(fields) != 2 {
		return runqueue.EnqueueResult{}, fmt.Errorf("%w: unexpected enqueue response %T", runqueue.ErrQueueUnavailable, result)
	}
	messageID, err := redisString(fields[0])
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	depth, err := redisInt64(fields[1])
	if err != nil {
		return runqueue.EnqueueResult{}, err
	}
	return runqueue.EnqueueResult{QueueName: message.QueueName, MessageID: messageID, Depth: depth}, nil
}

func (q *Queue) Dequeue(ctx context.Context, request runqueue.DequeueRequest) ([]runqueue.Lease, error) {
	if strings.TrimSpace(request.QueueName) == "" {
		return nil, errors.New("queue name is required")
	}
	if strings.TrimSpace(request.OrgID) == "" {
		return nil, errors.New("org id is required")
	}
	if strings.TrimSpace(request.WorkerGroupID) == "" {
		return nil, errors.New("worker group id is required")
	}
	if strings.TrimSpace(request.WorkerHostID) == "" {
		return nil, errors.New("worker host id is required")
	}
	if err := request.Available.Validate(false); err != nil {
		return nil, err
	}
	if request.Available.Slots <= 0 {
		return nil, compute.ErrNoCapacity
	}
	maxMessages := request.MaxMessages
	if maxMessages <= 0 {
		maxMessages = defaultMaxMessages
	}
	keys := q.keys(request.OrgID, request.WorkerGroupID, request.QueueName)
	labels, err := jsonMap(request.Labels)
	if err != nil {
		return nil, err
	}

	deadline := q.now().UTC().Add(request.Wait)
	for {
		now := q.now().UTC()
		expiresAt := now.Add(q.leaseTimeout)
		result, err := q.client.Eval(ctx, dequeueScript, []string{keys.ready, keys.active},
			q.prefix,
			now.UnixMilli(),
			q.leaseTimeout.Milliseconds(),
			maxMessages,
			request.WorkerHostID,
			request.Available.MilliCPU,
			request.Available.MemoryMiB,
			request.Available.DiskMiB,
			request.Available.Slots,
			defaultReclaim,
			defaultScanLimit,
			q.generationTTL.Milliseconds(),
			request.Runtime.Arch,
			request.Runtime.ABI,
			request.Runtime.KernelDigest,
			request.Runtime.RootfsDigest,
			request.Runtime.CNIProfile,
			request.Region,
			labels,
		).Result()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
		}
		rows, ok := result.([]interface{})
		if !ok {
			return nil, fmt.Errorf("%w: unexpected dequeue response %T", runqueue.ErrQueueUnavailable, result)
		}
		leases := make([]runqueue.Lease, 0, len(rows))
		for _, row := range rows {
			fields, ok := row.([]interface{})
			if !ok || len(fields) != 4 {
				return nil, fmt.Errorf("%w: unexpected dequeue row %T", runqueue.ErrQueueUnavailable, row)
			}
			leaseID, err := redisString(fields[0])
			if err != nil {
				return nil, err
			}
			messageID, err := redisString(fields[1])
			if err != nil {
				return nil, err
			}
			payload, err := redisBytes(fields[2])
			if err != nil {
				return nil, err
			}
			attempt, err := redisInt32(fields[3])
			if err != nil {
				return nil, err
			}
			var message runqueue.Message
			if err := json.Unmarshal(payload, &message); err != nil {
				return nil, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
			}
			leases = append(leases, runqueue.Lease{
				ID:            leaseID,
				MessageID:     messageID,
				Message:       message,
				WorkerHostID:  request.WorkerHostID,
				AttemptNumber: attempt,
				ExpiresAt:     expiresAt,
			})
		}
		if len(leases) > 0 || request.Wait <= 0 || !q.now().UTC().Before(deadline) {
			return leases, nil
		}
		pause := time.Until(deadline)
		if pause > 100*time.Millisecond {
			pause = 100 * time.Millisecond
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (q *Queue) ReadyMessageExists(ctx context.Context, messageID string) (bool, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		return false, errors.New("message id is required")
	}
	result, err := q.client.Eval(ctx, readyMessageExistsScript, []string{}, q.prefix, messageID, q.now().UTC().UnixMilli(), q.generationTTL.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
	}
	switch result {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%w: unexpected message exists result %d", runqueue.ErrQueueUnavailable, result)
	}
}

func (q *Queue) Ack(ctx context.Context, lease runqueue.Lease) error {
	return q.finishLease(ctx, lease, "ack", "")
}

func (q *Queue) Nack(ctx context.Context, lease runqueue.Lease, reason runqueue.NackReason) error {
	if reason == "" {
		reason = runqueue.NackReasonRetry
	}
	return q.finishLease(ctx, lease, "nack", string(reason))
}

func (q *Queue) Renew(ctx context.Context, lease runqueue.Lease, expiresAt time.Time) (runqueue.Lease, error) {
	if expiresAt.IsZero() {
		return runqueue.Lease{}, errors.New("lease expiry is required")
	}
	if strings.TrimSpace(lease.ID) == "" {
		return runqueue.Lease{}, errors.New("lease id is required")
	}
	if strings.TrimSpace(lease.WorkerHostID) == "" {
		return runqueue.Lease{}, errors.New("worker host id is required")
	}
	result, err := q.client.Eval(ctx, renewScript, []string{}, q.prefix, lease.ID, lease.WorkerHostID, q.now().UTC().UnixMilli(), expiresAt.UTC().UnixMilli(), q.generationTTL.Milliseconds()).Int()
	if err != nil {
		return runqueue.Lease{}, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
	}
	switch result {
	case 1:
		lease.ExpiresAt = expiresAt.UTC()
		return lease, nil
	case -1:
		return runqueue.Lease{}, runqueue.ErrMessageNotFound
	case -2:
		return runqueue.Lease{}, runqueue.ErrLeaseConflict
	case -3:
		return runqueue.Lease{}, runqueue.ErrLeaseExpired
	default:
		return runqueue.Lease{}, fmt.Errorf("%w: unexpected renew result %d", runqueue.ErrQueueUnavailable, result)
	}
}

func (q *Queue) finishLease(ctx context.Context, lease runqueue.Lease, action string, reason string) error {
	if strings.TrimSpace(lease.ID) == "" {
		return errors.New("lease id is required")
	}
	if strings.TrimSpace(lease.WorkerHostID) == "" {
		return errors.New("worker host id is required")
	}
	result, err := q.client.Eval(ctx, finishScript, []string{}, q.prefix, lease.ID, lease.WorkerHostID, q.now().UTC().UnixMilli(), action, reason, q.generationTTL.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
	}
	switch result {
	case 1:
		return nil
	case -1:
		return runqueue.ErrMessageNotFound
	case -2:
		return runqueue.ErrLeaseConflict
	case -3:
		return runqueue.ErrLeaseExpired
	default:
		return fmt.Errorf("%w: unexpected finish result %d", runqueue.ErrQueueUnavailable, result)
	}
}

type queueKeys struct {
	scope       string
	orgRunScope string
	ready       string
	active      string
}

func (q *Queue) keys(orgID string, workerGroupID string, queueName string) queueKeys {
	orgScope := "org:" + sanitizeKeyPart(orgID)
	scope := orgScope + ":group:" + sanitizeKeyPart(workerGroupID) + ":queue:" + sanitizeKeyPart(queueName)
	base := q.prefix + ":" + scope
	return queueKeys{
		scope:       scope,
		orgRunScope: q.prefix + ":" + orgScope,
		ready:       base + ":ready",
		active:      base + ":active",
	}
}

func sanitizeKeyPart(value string) string {
	return strings.NewReplacer(":", "_", "{", "_", "}", "_", "\n", "_", "\r", "_").Replace(value)
}

func readyScore(priority int32, enqueuedAt time.Time) float64 {
	return float64(-priority)*1_000_000_000_000 + float64(enqueuedAt.UTC().UnixMilli())
}

func jsonMap(values map[string]string) (string, error) {
	if len(values) == 0 {
		return "", nil
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func redisString(value interface{}) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		return "", fmt.Errorf("%w: unexpected redis string %T", runqueue.ErrQueueUnavailable, value)
	}
}

func redisBytes(value interface{}) ([]byte, error) {
	switch v := value.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		return nil, fmt.Errorf("%w: unexpected redis bytes %T", runqueue.ErrQueueUnavailable, value)
	}
}

func redisInt32(value interface{}) (int32, error) {
	parsed, err := redisInt64(value)
	if err != nil {
		return 0, err
	}
	if parsed > math.MaxInt32 || parsed < math.MinInt32 {
		return 0, fmt.Errorf("%w: redis integer overflows int32", runqueue.ErrQueueUnavailable)
	}
	return int32(parsed), nil
}

func redisInt64(value interface{}) (int64, error) {
	switch v := value.(type) {
	case int64:
		return v, nil
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%w: %v", runqueue.ErrQueueUnavailable, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%w: unexpected redis integer %T", runqueue.ErrQueueUnavailable, value)
	}
}
