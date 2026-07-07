package control

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	workspaceCreateClaimTTL         = 30 * time.Second
	workspaceCreateClaimResolvedTTL = 5 * time.Minute
	workspaceCreateClaimPrefix      = "helmr:workspace-create:"
)

var errWorkspaceCreateCoordinationUnavailable = codedError{code: "coordination_unavailable"}

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
	status, err := claimSessionStartKey(ctx, s.eventStream.redis, key, owner, sessionStartClaimBoundedTTL(expiresAt, workspaceCreateClaimTTL))
	if err != nil {
		return workspaceCreateClaim{}, errWorkspaceCreateCoordinationUnavailable
	}
	if status == sessionStartClaimKeyResolved {
		return workspaceCreateClaim{server: s, key: key, resolved: true}, nil
	}
	if status != sessionStartClaimKeyAcquired {
		return workspaceCreateClaim{}, errWorkspaceOperationPending
	}
	return workspaceCreateClaim{
		server:      s,
		key:         key,
		owner:       owner,
		resolvedTTL: sessionStartClaimBoundedTTL(expiresAt, workspaceCreateClaimResolvedTTL),
		active:      true,
	}, nil
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
