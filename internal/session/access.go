package session

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type sessionOrigin struct {
	Mode, Kind            string
	User, Key, Activation *uuid.UUID
	Agent                 uuid.UUID
}

func (o sessionOrigin) id() uuid.UUID {
	if o.User != nil {
		return *o.User
	}
	if o.Key != nil {
		return *o.Key
	}
	if o.Activation != nil {
		return *o.Activation
	}
	return uuid.Nil()
}
func loadSessionOrigin(ctx context.Context, q db.DBTX, env, id uuid.UUID) (sessionOrigin, error) {
	var o sessionOrigin
	err := q.QueryRow(ctx, `SELECT admission_mode,origin_kind,origin_user_id,origin_api_key_id,origin_activation_id,agent_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, id).Scan(&o.Mode, &o.Kind, &o.User, &o.Key, &o.Activation, &o.Agent)
	return o, hideMissing(err)
}
func accessError(err error) error {
	if errors.Is(err, access.ErrDenied) || errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}
func requireCapability(ctx context.Context, q db.DBTX, kind string, id, env, agent uuid.UUID, permission auth.Permission) error {
	return accessError(access.Check(ctx, q, kind, id, env, agent, permission))
}
func (o sessionOrigin) authorize(ctx context.Context, q db.DBTX, env, agent uuid.UUID) error {
	if o.Kind == "schedule" {
		if o.Mode != "environment" || o.Activation == nil {
			return ErrDenied
		}
		var allowed bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM trigger_activations WHERE environment_id=$1 AND id=$2 AND (active_until IS NULL OR active_until>statement_timestamp()))`, env, *o.Activation).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrDenied
		}
		return nil
	}
	permission := auth.CapabilityRunsOperate
	if o.Mode == "scoped" {
		permission = auth.CapabilityAgentsUse
	} else if o.Mode != "environment" {
		return ErrDenied
	}
	return requireCapability(ctx, q, o.Kind, o.id(), env, agent, permission)
}
func resolveStartOrigin(ctx context.Context, tx pgx.Tx, caller Caller, req StartRequest, agent uuid.UUID) (sessionOrigin, error) {
	var o sessionOrigin
	if caller.Kind == "session" {
		var err error
		o, err = loadSessionOrigin(ctx, tx, req.EnvironmentID, caller.ID)
		if err != nil {
			return o, err
		}
		if err = o.authorize(ctx, tx, req.EnvironmentID, o.Agent); err != nil {
			return o, err
		}
	} else {
		p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, req.EnvironmentID)
		if err != nil {
			return o, accessError(err)
		}
		scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: agent.String()}
		o = sessionOrigin{Mode: "scoped", Kind: caller.Kind, Agent: agent}
		if caller.Kind == "user" {
			id := caller.ID
			o.User = &id
		} else if caller.Kind == "api_key" {
			id := caller.ID
			o.Key = &id
		} else {
			return o, ErrDenied
		}
		if p.HasPermission(auth.CapabilityRunsOperate, scope) {
			o.Mode = "environment"
		}
	}
	if err := o.authorize(ctx, tx, req.EnvironmentID, agent); err != nil {
		return o, err
	}
	if o.Mode == "scoped" {
		if req.ComputerID != uuid.Nil() || req.SessionKey != nil || req.SlackChannelID != nil {
			return o, ErrDenied
		}
		if o.Kind == "api_key" && req.IdempotencyKey == "" {
			return o, ErrInvalidInput
		}
	}
	return o, nil
}

// authorizeTarget is used before target reads, mutations and receipt returns.
// Runtime relations are additional constraints, never a replacement for origin.
func authorizeTarget(ctx context.Context, tx pgx.Tx, caller Caller, env, target uuid.UUID, operation auth.Permission) error {
	o, err := loadSessionOrigin(ctx, tx, env, target)
	if err != nil {
		return err
	}
	if caller.Kind != "session" {
		p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, env)
		if err != nil {
			return accessError(err)
		}
		scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: o.Agent.String()}
		if p.HasPermission(operation, scope) {
			return nil
		}
		if !ownSessionOperation(operation) || o.Mode != "scoped" || o.Kind != caller.Kind || o.id() != caller.ID {
			return ErrDenied
		}
		if !p.HasPermission(auth.CapabilityAgentsUse, scope) {
			return ErrDenied
		}
		return nil
	}
	source, err := loadSessionOrigin(ctx, tx, env, caller.ID)
	if err != nil {
		return err
	}
	if err = source.authorize(ctx, tx, env, source.Agent); err != nil {
		return err
	}
	if source.Mode == "environment" {
		if o.Mode == "scoped" && source.Kind == "schedule" {
			return ErrDenied
		}
		return nil
	}
	if o.Mode != "scoped" || o.Kind != source.Kind || o.id() != source.id() {
		return ErrDenied
	}
	if err = source.authorize(ctx, tx, env, o.Agent); err != nil {
		return err
	}
	var related bool
	err = tx.QueryRow(ctx, `WITH RECURSIVE descendants AS (
 SELECT id FROM sessions WHERE environment_id=$1 AND id=$2
 UNION ALL SELECT s.id FROM sessions s JOIN descendants d ON s.parent_session_id=d.id WHERE s.environment_id=$1
 ) SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND id=$3 AND (id IN (SELECT id FROM descendants) OR (parent_session_id IS NULL AND requester_session_id=$2)))`, env, caller.ID, target).Scan(&related)
	if err != nil {
		return err
	}
	if !related {
		return ErrDenied
	}
	return nil
}

// The Session origin governs future runtime admission. Already admitted native
// effects and cleanup observations do not acquire new authority through this check.
func authorizeRuntimeOrigin(ctx context.Context, tx pgx.Tx, env, id uuid.UUID) error {
	o, err := loadSessionOrigin(ctx, tx, env, id)
	if err != nil {
		return err
	}
	return o.authorize(ctx, tx, env, o.Agent)
}

type sessionReadScope struct {
	All, AllAgents bool
	Kind           string
	ID             uuid.UUID
	Agents         []uuid.UUID
}

func readScopeFor(ctx context.Context, q db.DBTX, caller Caller, env uuid.UUID) (sessionReadScope, error) {
	filter := sessionReadScope{Kind: caller.Kind, ID: caller.ID, Agents: []uuid.UUID{}}
	p, err := access.Principal(ctx, q, caller.Kind, caller.ID, env)
	if err != nil {
		return filter, accessError(err)
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
	filter.All = p.HasPermission(auth.PermissionSessionsRead, scope)
	filter.AllAgents = p.HasPermission(auth.CapabilityAgentsUse, scope)
	for _, g := range p.Grants {
		if g.EnvironmentID == env.String() && g.AgentID != "" && g.Permission == auth.CapabilityAgentsUse {
			if id, err := uuid.Parse(g.AgentID); err == nil {
				filter.Agents = append(filter.Agents, id)
			}
		}
	}
	// An active principal with no grant gets an empty list, never an unfiltered one.
	return filter, nil
}
func authorizeSessionRead(ctx context.Context, tx pgx.Tx, caller Caller, env, id uuid.UUID) error {
	return authorizeTarget(ctx, tx, caller, env, id, auth.PermissionSessionsRead)
}

func ownSessionOperation(operation auth.Permission) bool {
	switch operation {
	case auth.PermissionSessionsRead, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsCancel, auth.PermissionSessionsInterrupt, auth.PermissionSessionsResume, auth.PermissionAsksAnswer:
		return true
	}
	return false
}

// Recheck the immutable Session origin and the caller that supplied deferred input.
func authorizeDeferredInput(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, caller Caller) error {
	if err := authorizeRuntimeOrigin(ctx, tx, env, session); err != nil {
		return err
	}
	if caller.Kind == "schedule" {
		return nil
	}
	return authorizeTarget(ctx, tx, caller, env, session, auth.PermissionSessionsSend)
}

// RuntimeAgentScope is used only after LockRuntimeCaller, in the same transaction.
// It preserves scoped lineage even when its principal later acquires broader access.
func RuntimeAgentScope(ctx context.Context, tx pgx.Tx, caller Caller) (bool, []uuid.UUID, error) {
	o, err := loadSessionOrigin(ctx, tx, caller.Execution.EnvironmentID, caller.ID)
	if err != nil {
		return false, nil, err
	}
	if err = o.authorize(ctx, tx, caller.Execution.EnvironmentID, o.Agent); err != nil {
		return false, nil, err
	}
	if o.Mode == "environment" {
		return true, []uuid.UUID{}, nil
	}
	scope, err := readScopeFor(ctx, tx, Caller{Kind: o.Kind, ID: o.id()}, caller.Execution.EnvironmentID)
	return scope.AllAgents, scope.Agents, err
}

func failQueuedPermission(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp(),error_code='permission_revoked',error_message=$3 WHERE environment_id=$1 AND session_id=$2 AND status='queued' RETURNING id`, env, session, []byte("Permission was revoked before execution"))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = eventData(ctx, tx, env, session, id, "turn.failed", []byte(`{"code":"permission_revoked"}`)); err != nil {
			return err
		}
	}
	if len(ids) > 0 {
		return notifySessionsChanged(ctx, tx, env, session)
	}
	return nil
}

func externalSessionPermissions(ctx context.Context, tx pgx.Tx, caller Caller, env, id uuid.UUID) ([]string, error) {
	result := []string{}
	o, err := loadSessionOrigin(ctx, tx, env, id)
	if err != nil {
		return nil, err
	}
	p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, env)
	if err != nil {
		return nil, accessError(err)
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: o.Agent.String()}
	own := o.Mode == "scoped" && o.Kind == caller.Kind && o.id() == caller.ID && p.HasPermission(auth.CapabilityAgentsUse, scope)
	for _, operation := range auth.AllPermissions() {
		if ownSessionOperation(operation) && (p.HasPermission(operation, scope) || own) {
			result = append(result, string(operation))
		}
	}
	return result, nil
}
