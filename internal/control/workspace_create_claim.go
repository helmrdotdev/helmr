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
	workspaceCreateClaimTTL         = 30 * time.Second
	workspaceCreateClaimResolvedTTL = 5 * time.Minute
	workspaceCreateClaimPrefix      = "helmr:workspace-create:"
)

var errWorkspaceCreateCoordinationUnavailable = codedError{code: "coordination_unavailable"}

type workspaceCreateClaimKeyStatus string

const (
	workspaceCreateClaimKeyAcquired workspaceCreateClaimKeyStatus = "acquired"
	workspaceCreateClaimKeyResolved workspaceCreateClaimKeyStatus = "resolved"
	workspaceCreateClaimKeyPending  workspaceCreateClaimKeyStatus = "pending"
)

type workspaceCreateClaim struct {
	server      *Server
	key         string
	owner       string
	resolvedTTL time.Duration
	active      bool
	resolved    bool
}

func (s *Server) claimWorkspaceCreate(ctx context.Context, orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, idempotencyKey string, expiresAt pgtype.Timestamptz) (workspaceCreateClaim, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" {
		return workspaceCreateClaim{}, nil
	}
	if s.eventStream == nil || s.eventStream.redis == nil {
		return workspaceCreateClaim{}, errWorkspaceCreateCoordinationUnavailable
	}
	key := workspaceCreateClaimKey(orgID, projectID, environmentID, idempotencyKey)
	owner := uuid.Must(uuid.NewV7()).String()
	status, err := claimWorkspaceCreateKey(ctx, s.eventStream.redis, key, owner, workspaceCreateClaimBoundedTTL(expiresAt, workspaceCreateClaimTTL))
	if err != nil {
		return workspaceCreateClaim{}, errWorkspaceCreateCoordinationUnavailable
	}
	if status == workspaceCreateClaimKeyResolved {
		return workspaceCreateClaim{server: s, key: key, resolved: true}, nil
	}
	if status != workspaceCreateClaimKeyAcquired {
		return workspaceCreateClaim{}, errWorkspaceOperationPending
	}
	return workspaceCreateClaim{
		server:      s,
		key:         key,
		owner:       owner,
		resolvedTTL: workspaceCreateClaimBoundedTTL(expiresAt, workspaceCreateClaimResolvedTTL),
		active:      true,
	}, nil
}

func claimWorkspaceCreateKey(ctx context.Context, redisClient redis.Cmdable, key string, owner string, ttl time.Duration) (workspaceCreateClaimKeyStatus, error) {
	for range 2 {
		claimed, err := redisClient.SetNX(ctx, key, "pending:"+owner, ttl).Result()
		if err != nil {
			return workspaceCreateClaimKeyPending, errWorkspaceCreateCoordinationUnavailable
		}
		if claimed {
			return workspaceCreateClaimKeyAcquired, nil
		}
		value, err := redisClient.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			return workspaceCreateClaimKeyPending, errWorkspaceCreateCoordinationUnavailable
		}
		if strings.HasPrefix(value, "resolved:") {
			return workspaceCreateClaimKeyResolved, nil
		}
		return workspaceCreateClaimKeyPending, nil
	}
	return workspaceCreateClaimKeyPending, nil
}

func workspaceCreateClaimBoundedTTL(expiresAt pgtype.Timestamptz, limit time.Duration) time.Duration {
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

func workspaceCreateClaimKey(orgID uuid.UUID, projectID pgtype.UUID, environmentID pgtype.UUID, idempotencyKey string) string {
	project := pgvalue.MustUUIDValue(projectID).String()
	environment := pgvalue.MustUUIDValue(environmentID).String()
	digest := sha256.Sum256([]byte(strings.TrimSpace(idempotencyKey)))
	return fmt.Sprintf("%s%s:%s:%s:%x", workspaceCreateClaimPrefix, orgID.String(), project, environment, digest[:])
}

func (c workspaceCreateClaim) resolve(ctx context.Context) {
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
	_ = c.server.eventStream.redis.Eval(ctx, script, []string{c.key}, "pending:"+c.owner, "resolved:"+c.owner, int64(c.resolvedTTL/time.Millisecond)).Err()
}

func (c workspaceCreateClaim) clearResolved(ctx context.Context) {
	if c.key == "" || c.server == nil || c.server.eventStream == nil || c.server.eventStream.redis == nil {
		return
	}
	const script = `
local value = redis.call("GET", KEYS[1])
if value and string.sub(value, 1, 9) == "resolved:" then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	_ = c.server.eventStream.redis.Eval(ctx, script, []string{c.key}).Err()
}

func (c workspaceCreateClaim) release(ctx context.Context) {
	if !c.active || c.server == nil || c.server.eventStream == nil || c.server.eventStream.redis == nil {
		return
	}
	const script = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0
`
	_ = c.server.eventStream.redis.Eval(ctx, script, []string{c.key}, "pending:"+c.owner).Err()
}
