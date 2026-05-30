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
	dispatch "github.com/helmrdotdev/helmr/internal/dispatch"
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

func (q *Queue) Enqueue(ctx context.Context, message dispatch.Message) (dispatch.EnqueueResult, error) {
	if message.EnqueuedAt.IsZero() {
		message.EnqueuedAt = q.now().UTC()
	}
	if message.QueueTimestamp.IsZero() {
		message.QueueTimestamp = message.EnqueuedAt
	}
	if err := message.Validate(); err != nil {
		return dispatch.EnqueueResult{}, err
	}
	payload, err := json.Marshal(message)
	if err != nil {
		return dispatch.EnqueueResult{}, err
	}
	placementLabels, err := jsonMap(message.Requirements.Placement.Tags)
	if err != nil {
		return dispatch.EnqueueResult{}, err
	}
	keys := q.keys(message.OrgID, message.QueueName)
	score := readyScore(message.Priority, message.QueueTimestamp)
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
		return dispatch.EnqueueResult{}, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
	}
	fields, ok := result.([]interface{})
	if !ok || len(fields) != 2 {
		return dispatch.EnqueueResult{}, fmt.Errorf("%w: unexpected enqueue response %T", dispatch.ErrQueueUnavailable, result)
	}
	messageID, err := redisString(fields[0])
	if err != nil {
		return dispatch.EnqueueResult{}, err
	}
	depth, err := redisInt64(fields[1])
	if err != nil {
		return dispatch.EnqueueResult{}, err
	}
	return dispatch.EnqueueResult{QueueName: message.QueueName, MessageID: messageID, Depth: depth}, nil
}

func (q *Queue) Dequeue(ctx context.Context, request dispatch.DequeueRequest) ([]dispatch.Lease, error) {
	if strings.TrimSpace(request.QueueName) == "" {
		return nil, errors.New("queue name is required")
	}
	if strings.TrimSpace(request.OrgID) == "" {
		return nil, errors.New("org id is required")
	}
	if strings.TrimSpace(request.WorkerInstanceID) == "" {
		return nil, errors.New("worker instance id is required")
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
	keys := q.keys(request.OrgID, request.QueueName)
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
			request.WorkerInstanceID,
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
			return nil, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
		}
		rows, ok := result.([]interface{})
		if !ok {
			return nil, fmt.Errorf("%w: unexpected dequeue response %T", dispatch.ErrQueueUnavailable, result)
		}
		leases := make([]dispatch.Lease, 0, len(rows))
		for _, row := range rows {
			fields, ok := row.([]interface{})
			if !ok || len(fields) != 4 {
				return nil, fmt.Errorf("%w: unexpected dequeue row %T", dispatch.ErrQueueUnavailable, row)
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
			var message dispatch.Message
			if err := json.Unmarshal(payload, &message); err != nil {
				return nil, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
			}
			leases = append(leases, dispatch.Lease{
				ID:               leaseID,
				MessageID:        messageID,
				Message:          message,
				WorkerInstanceID: request.WorkerInstanceID,
				AttemptNumber:    attempt,
				ExpiresAt:        expiresAt,
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
		return false, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
	}
	switch result {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, fmt.Errorf("%w: unexpected message exists result %d", dispatch.ErrQueueUnavailable, result)
	}
}

func (q *Queue) Ack(ctx context.Context, lease dispatch.Lease) error {
	return q.finishLease(ctx, lease, "ack", "")
}

func (q *Queue) Nack(ctx context.Context, lease dispatch.Lease, reason dispatch.NackReason) error {
	if reason == "" {
		reason = dispatch.NackReasonRetry
	}
	return q.finishLease(ctx, lease, "nack", string(reason))
}

func (q *Queue) Renew(ctx context.Context, lease dispatch.Lease, expiresAt time.Time) (dispatch.Lease, error) {
	if expiresAt.IsZero() {
		return dispatch.Lease{}, errors.New("lease expiry is required")
	}
	if strings.TrimSpace(lease.ID) == "" {
		return dispatch.Lease{}, errors.New("lease id is required")
	}
	if strings.TrimSpace(lease.WorkerInstanceID) == "" {
		return dispatch.Lease{}, errors.New("worker instance id is required")
	}
	result, err := q.client.Eval(ctx, renewScript, []string{}, q.prefix, lease.ID, lease.WorkerInstanceID, q.now().UTC().UnixMilli(), expiresAt.UTC().UnixMilli(), q.generationTTL.Milliseconds()).Int()
	if err != nil {
		return dispatch.Lease{}, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
	}
	switch result {
	case 1:
		lease.ExpiresAt = expiresAt.UTC()
		return lease, nil
	case -1:
		return dispatch.Lease{}, dispatch.ErrMessageNotFound
	case -2:
		return dispatch.Lease{}, dispatch.ErrLeaseConflict
	case -3:
		return dispatch.Lease{}, dispatch.ErrLeaseExpired
	default:
		return dispatch.Lease{}, fmt.Errorf("%w: unexpected renew result %d", dispatch.ErrQueueUnavailable, result)
	}
}

func (q *Queue) finishLease(ctx context.Context, lease dispatch.Lease, action string, reason string) error {
	if strings.TrimSpace(lease.ID) == "" {
		return errors.New("lease id is required")
	}
	if strings.TrimSpace(lease.WorkerInstanceID) == "" {
		return errors.New("worker instance id is required")
	}
	result, err := q.client.Eval(ctx, finishScript, []string{}, q.prefix, lease.ID, lease.WorkerInstanceID, q.now().UTC().UnixMilli(), action, reason, q.generationTTL.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
	}
	switch result {
	case 1:
		return nil
	case -1:
		return dispatch.ErrMessageNotFound
	case -2:
		return dispatch.ErrLeaseConflict
	case -3:
		return dispatch.ErrLeaseExpired
	default:
		return fmt.Errorf("%w: unexpected finish result %d", dispatch.ErrQueueUnavailable, result)
	}
}

type queueKeys struct {
	scope       string
	orgRunScope string
	ready       string
	active      string
}

func (q *Queue) keys(orgID string, queueName string) queueKeys {
	orgScope := "org:" + sanitizeKeyPart(orgID)
	scope := orgScope + ":queue:" + sanitizeKeyPart(queueName)
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
	return float64(enqueuedAt.UTC().UnixMilli() - int64(priority)*1000)
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
		return "", fmt.Errorf("%w: unexpected redis string %T", dispatch.ErrQueueUnavailable, value)
	}
}

func redisBytes(value interface{}) ([]byte, error) {
	switch v := value.(type) {
	case string:
		return []byte(v), nil
	case []byte:
		return v, nil
	default:
		return nil, fmt.Errorf("%w: unexpected redis bytes %T", dispatch.ErrQueueUnavailable, value)
	}
}

func redisInt32(value interface{}) (int32, error) {
	parsed, err := redisInt64(value)
	if err != nil {
		return 0, err
	}
	if parsed > math.MaxInt32 || parsed < math.MinInt32 {
		return 0, fmt.Errorf("%w: redis integer overflows int32", dispatch.ErrQueueUnavailable)
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
			return 0, fmt.Errorf("%w: %v", dispatch.ErrQueueUnavailable, err)
		}
		return parsed, nil
	default:
		return 0, fmt.Errorf("%w: unexpected redis integer %T", dispatch.ErrQueueUnavailable, value)
	}
}
