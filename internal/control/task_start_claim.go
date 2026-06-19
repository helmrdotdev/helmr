package control

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/redis/go-redis/v9"
)

const (
	taskStartClaimTTL         = 30 * time.Second
	taskStartClaimResolvedTTL = 5 * time.Minute
	taskStartClaimPrefix      = "helmr:task-start:"
)

var (
	errTaskStartPending                 = codedError{code: "idempotency_pending", message: "task_start_pending"}
	errTaskStartCoordinationUnavailable = codedError{code: "coordination_unavailable"}
)

type taskStartClaimKeyStatus string

const (
	taskStartClaimKeyAcquired taskStartClaimKeyStatus = "acquired"
	taskStartClaimKeyResolved taskStartClaimKeyStatus = "resolved"
	taskStartClaimKeyPending  taskStartClaimKeyStatus = "pending"
)

type taskStartClaim struct {
	server      *Server
	keys        []string
	owner       string
	resolvedTTL time.Duration
	active      bool
	resolved    bool
}

func (s *Server) claimTaskStart(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, idempotencyKey string, expiresAt pgtype.Timestamptz) (taskStartClaim, error) {
	claim := taskStartClaim{}
	keys := taskStartClaimKeys(orgID, projectID, environmentID, taskID, idempotencyKey)
	if len(keys) == 0 {
		return claim, nil
	}
	if s.eventStream == nil || s.eventStream.redis == nil {
		return claim, errTaskStartCoordinationUnavailable
	}
	owner := uuid.Must(uuid.NewV7()).String()
	pendingTTL := taskStartClaimBoundedTTL(expiresAt, taskStartClaimTTL)
	resolvedTTL := taskStartClaimBoundedTTL(expiresAt, taskStartClaimResolvedTTL)
	claimedKeys := make([]string, 0, len(keys))
	for _, key := range keys {
		status, err := claimTaskStartKey(ctx, s.eventStream.redis, key, owner, pendingTTL)
		if err != nil {
			taskStartClaim{server: s, keys: claimedKeys, owner: owner, active: len(claimedKeys) > 0}.release(ctx)
			return claim, err
		}
		if status == taskStartClaimKeyResolved {
			taskStartClaim{server: s, keys: claimedKeys, owner: owner, active: len(claimedKeys) > 0}.release(ctx)
			return taskStartClaim{server: s, keys: keys, resolved: true}, nil
		}
		if status != taskStartClaimKeyAcquired {
			taskStartClaim{server: s, keys: claimedKeys, owner: owner, active: len(claimedKeys) > 0}.release(ctx)
			return claim, errTaskStartPending
		}
		claimedKeys = append(claimedKeys, key)
	}
	return taskStartClaim{server: s, keys: claimedKeys, owner: owner, active: true, resolvedTTL: resolvedTTL}, nil
}

func claimTaskStartKey(ctx context.Context, redisClient redis.Cmdable, key string, owner string, ttl time.Duration) (taskStartClaimKeyStatus, error) {
	for attempt := 0; attempt < 2; attempt++ {
		claimed, err := redisClient.SetNX(ctx, key, "pending:"+owner, ttl).Result()
		if err != nil {
			return taskStartClaimKeyPending, errTaskStartCoordinationUnavailable
		}
		if claimed {
			return taskStartClaimKeyAcquired, nil
		}
		value, err := redisClient.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return taskStartClaimKeyPending, errTaskStartCoordinationUnavailable
		}
		if strings.HasPrefix(value, "resolved:") {
			return taskStartClaimKeyResolved, nil
		}
		return taskStartClaimKeyPending, nil
	}
	return taskStartClaimKeyPending, nil
}

func taskStartClaimBoundedTTL(expiresAt pgtype.Timestamptz, limit time.Duration) time.Duration {
	if !expiresAt.Valid {
		return limit
	}
	remaining := time.Until(expiresAt.Time)
	if remaining <= 0 {
		return time.Millisecond
	}
	if remaining < limit {
		return remaining
	}
	return limit
}

func taskStartClaimKeys(orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, idempotencyKey string) []string {
	if idempotencyKey = strings.TrimSpace(idempotencyKey); idempotencyKey != "" {
		return []string{taskStartClaimKey(orgID, projectID, environmentID, taskID, "idempotency", idempotencyKey)}
	}
	return nil
}

func taskStartClaimKey(orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, taskID string, identityKind string, identityValue string) string {
	project := pgvalue.MustUUIDValue(projectID).String()
	environment := pgvalue.MustUUIDValue(environmentID).String()
	digest := sha256.Sum256([]byte(strings.Join([]string{
		taskID,
		strings.TrimSpace(identityKind),
		strings.TrimSpace(identityValue),
	}, "\x00")))
	return fmt.Sprintf("%s%s:%s:%s:%x", taskStartClaimPrefix, orgID.String(), project, environment, digest[:])
}

func (c taskStartClaim) resolve(ctx context.Context) {
	if !c.active || c.server == nil || c.server.eventStream == nil || c.server.eventStream.redis == nil {
		return
	}
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  redis.call("SET", KEYS[1], ARGV[2], "PX", ARGV[3])
  return 1
end
return 0
`
	for _, key := range c.keys {
		_ = c.server.eventStream.redis.Eval(ctx, script, []string{key}, "pending:"+c.owner, "resolved:"+c.owner, int64(c.resolvedTTL/time.Millisecond)).Err()
	}
}

func (c taskStartClaim) clearResolved(ctx context.Context) {
	if len(c.keys) == 0 || c.server == nil || c.server.eventStream == nil || c.server.eventStream.redis == nil {
		return
	}
	const script = `
local value = redis.call("GET", KEYS[1])
if value and string.sub(value, 1, 9) == "resolved:" then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	for _, key := range c.keys {
		_ = c.server.eventStream.redis.Eval(ctx, script, []string{key}).Err()
	}
}

func (c taskStartClaim) release(ctx context.Context) {
	if !c.active || c.server == nil || c.server.eventStream == nil || c.server.eventStream.redis == nil {
		return
	}
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	for _, key := range c.keys {
		_ = c.server.eventStream.redis.Eval(ctx, script, []string{key}, "pending:"+c.owner).Err()
	}
}
