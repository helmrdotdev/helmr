package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

const (
	defaultIndexPrefix = "helmr:schedule"
	defaultReclaim     = int32(128)
)

type IndexEntry struct {
	CellID      string
	InstanceID  uuid.UUID
	Generation  int64
	ScheduledAt time.Time
	AvailableAt time.Time
}

type DequeueRequest struct {
	CellID   string
	WorkerID uuid.UUID
	Limit    int32
	Now      time.Time
	Lease    time.Duration
}

type IndexLease struct {
	ID        string
	MessageID string
	Entry     IndexEntry
	Payload   string
	Attempt   int32
	ExpiresAt time.Time
	WorkerID  uuid.UUID
}

type RedisIndex struct {
	client redis.Cmdable
	prefix string
	now    func() time.Time
}

type RedisIndexOption func(*RedisIndex)

func NewRedisIndex(client redis.Cmdable, opts ...RedisIndexOption) (*RedisIndex, error) {
	if client == nil {
		return nil, errors.New("redis client is required")
	}
	index := &RedisIndex{
		client: client,
		prefix: defaultIndexPrefix,
		now:    func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		opt(index)
	}
	if strings.TrimSpace(index.prefix) == "" {
		return nil, errors.New("redis key prefix is required")
	}
	if index.now == nil {
		return nil, errors.New("redis clock is required")
	}
	index.prefix = strings.TrimRight(index.prefix, ":")
	return index, nil
}

func WithRedisIndexClock(now func() time.Time) RedisIndexOption {
	return func(index *RedisIndex) {
		index.now = now
	}
}

func (i *RedisIndex) Enqueue(ctx context.Context, entry IndexEntry) error {
	cellID := normalizedCellID(entry.CellID)
	if cellID == "" {
		return errors.New("schedule cell id is required")
	}
	if entry.InstanceID == uuid.Nil {
		return errors.New("schedule instance id is required")
	}
	if entry.Generation <= 0 {
		return errors.New("schedule generation is required")
	}
	if entry.ScheduledAt.IsZero() {
		return errors.New("scheduled time is required")
	}
	if entry.AvailableAt.IsZero() {
		entry.AvailableAt = entry.ScheduledAt
	}
	payload, err := json.Marshal(entryPayload{
		CellID:      cellID,
		InstanceID:  entry.InstanceID.String(),
		Generation:  entry.Generation,
		ScheduledAt: entry.ScheduledAt.UTC().Format(time.RFC3339Nano),
		AvailableAt: entry.AvailableAt.UTC().Format(time.RFC3339Nano),
	})
	if err != nil {
		return err
	}
	messageID := indexMessageID(entry)
	prefix := i.cellPrefix(cellID)
	_, err = i.client.Eval(ctx, scheduleEnqueueScript, []string{i.readyKey(prefix)},
		prefix,
		messageID,
		string(payload),
		entry.AvailableAt.UTC().UnixMilli(),
	).Result()
	if err != nil {
		return fmt.Errorf("enqueue schedule index: %w", err)
	}
	return nil
}

func (i *RedisIndex) Delete(ctx context.Context, cellID string, instanceID uuid.UUID) error {
	cellID = normalizedCellID(cellID)
	if cellID == "" {
		return errors.New("schedule cell id is required")
	}
	if instanceID == uuid.Nil {
		return errors.New("schedule instance id is required")
	}
	prefix := i.cellPrefix(cellID)
	messageID := indexMessageID(IndexEntry{CellID: cellID, InstanceID: instanceID})
	if err := i.client.Eval(ctx, scheduleDeleteScript, []string{i.readyKey(prefix), i.activeKey(prefix)},
		prefix,
		messageID,
	).Err(); err != nil {
		return fmt.Errorf("delete schedule index entry: %w", err)
	}
	return nil
}

func (i *RedisIndex) Dequeue(ctx context.Context, request DequeueRequest) ([]IndexLease, error) {
	cellID := normalizedCellID(request.CellID)
	if cellID == "" {
		return nil, errors.New("schedule cell id is required")
	}
	if request.WorkerID == uuid.Nil {
		return nil, errors.New("worker id is required")
	}
	if request.Limit <= 0 {
		request.Limit = DefaultRepairLimit
	}
	if request.Lease <= 0 {
		return nil, errors.New("lease must be positive")
	}
	now := request.Now.UTC()
	if now.IsZero() {
		now = i.now()
	}
	expiresAt := now.Add(request.Lease)
	prefix := i.cellPrefix(cellID)
	result, err := i.client.Eval(ctx, scheduleDequeueScript, []string{i.readyKey(prefix), i.activeKey(prefix)},
		prefix,
		now.UnixMilli(),
		request.Lease.Milliseconds(),
		request.Limit,
		defaultReclaim,
		request.WorkerID.String(),
	).Result()
	if err != nil {
		return nil, fmt.Errorf("dequeue schedule index: %w", err)
	}
	rows, ok := result.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected schedule dequeue response %T", result)
	}
	leases := make([]IndexLease, 0, len(rows))
	for _, row := range rows {
		fields, ok := row.([]any)
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("unexpected schedule dequeue row %T", row)
		}
		leaseID, err := redisString(fields[0])
		if err != nil {
			return nil, err
		}
		messageID, err := redisString(fields[1])
		if err != nil {
			return nil, err
		}
		payload, err := redisString(fields[2])
		if err != nil {
			return nil, err
		}
		attempt, err := redisInt32(fields[3])
		if err != nil {
			return nil, err
		}
		entry, err := decodeEntry(payload)
		if err != nil {
			return nil, err
		}
		leases = append(leases, IndexLease{
			ID:        leaseID,
			MessageID: messageID,
			Entry:     entry,
			Payload:   payload,
			Attempt:   attempt,
			ExpiresAt: expiresAt,
			WorkerID:  request.WorkerID,
		})
	}
	return leases, nil
}

func (i *RedisIndex) Ack(ctx context.Context, lease IndexLease) error {
	return i.finish(ctx, lease, "ack", time.Time{})
}

func (i *RedisIndex) Nack(ctx context.Context, lease IndexLease, retryAt time.Time) error {
	return i.finish(ctx, lease, "nack", retryAt)
}

func (i *RedisIndex) finish(ctx context.Context, lease IndexLease, action string, retryAt time.Time) error {
	cellID := normalizedCellID(lease.Entry.CellID)
	if cellID == "" {
		return errors.New("schedule cell id is required")
	}
	if strings.TrimSpace(lease.ID) == "" {
		return errors.New("lease id is required")
	}
	if lease.WorkerID == uuid.Nil {
		return errors.New("worker id is required")
	}
	retryAtMs := int64(0)
	if !retryAt.IsZero() {
		retryAtMs = retryAt.UTC().UnixMilli()
	}
	prefix := i.cellPrefix(cellID)
	result, err := i.client.Eval(ctx, scheduleFinishScript, []string{},
		prefix,
		lease.ID,
		lease.WorkerID.String(),
		i.now().UTC().UnixMilli(),
		action,
		retryAtMs,
		lease.Payload,
	).Int()
	if err != nil {
		return fmt.Errorf("finish schedule lease: %w", err)
	}
	switch result {
	case 1:
		return nil
	case -1:
		return nil
	case -2:
		return errors.New("schedule lease is owned by another worker")
	case -3:
		return nil
	default:
		return fmt.Errorf("unexpected schedule finish result %d", result)
	}
}

func (i *RedisIndex) cellPrefix(cellID string) string {
	return i.prefix + ":cell:" + normalizedCellID(cellID)
}

func (i *RedisIndex) readyKey(prefix string) string {
	return prefix + ":ready"
}

func (i *RedisIndex) activeKey(prefix string) string {
	return prefix + ":active"
}

func indexMessageID(entry IndexEntry) string {
	return "instance:" + entry.InstanceID.String()
}

type entryPayload struct {
	CellID      string `json:"cell_id"`
	InstanceID  string `json:"instance_id"`
	Generation  int64  `json:"generation"`
	ScheduledAt string `json:"scheduled_at"`
	AvailableAt string `json:"available_at"`
}

func decodeEntry(payload string) (IndexEntry, error) {
	var decoded entryPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return IndexEntry{}, err
	}
	instanceID, err := uuid.Parse(decoded.InstanceID)
	if err != nil {
		return IndexEntry{}, err
	}
	scheduledAt, err := time.Parse(time.RFC3339Nano, decoded.ScheduledAt)
	if err != nil {
		return IndexEntry{}, err
	}
	availableAt, err := time.Parse(time.RFC3339Nano, decoded.AvailableAt)
	if err != nil {
		return IndexEntry{}, err
	}
	cellID := normalizedCellID(decoded.CellID)
	if cellID == "" {
		return IndexEntry{}, errors.New("schedule cell id is required")
	}
	return IndexEntry{
		CellID:      cellID,
		InstanceID:  instanceID,
		Generation:  decoded.Generation,
		ScheduledAt: scheduledAt.UTC(),
		AvailableAt: availableAt.UTC(),
	}, nil
}

func normalizedCellID(cellID string) string {
	return strings.TrimSpace(cellID)
}

func redisString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case []byte:
		return string(v), nil
	default:
		return "", fmt.Errorf("unexpected redis string %T", value)
	}
}

func redisInt32(value any) (int32, error) {
	switch v := value.(type) {
	case int64:
		return int32(v), nil
	case string:
		parsed, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return 0, err
		}
		return int32(parsed), nil
	default:
		return 0, fmt.Errorf("unexpected redis integer %T", value)
	}
}
