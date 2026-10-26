package session

import (
	"context"
	"errors"
	"github.com/helmrdotdev/helmr/internal/auditpolicy"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type sessionOrigin struct {
	User, Key, Activation *uuid.UUID
	Connection            *uuid.UUID
	Agent                 uuid.UUID
}

// The typed origin references are immutable; admission labels are derived.
func (o sessionOrigin) mode() string {
	if o.Connection != nil {
		return "shared"
	}
	if o.Activation != nil {
		return "environment"
	}
	return "scoped"
}

func (o sessionOrigin) kind() string {
	if o.User != nil {
		return "user"
	}
	if o.Key != nil {
		return "api_key"
	}
	if o.Activation != nil {
		return "schedule"
	}
	return ""
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
	err := q.QueryRow(ctx, `SELECT origin_user_id,origin_api_key_id,origin_activation_id,agent_id,origin_slack_connection_id FROM sessions WHERE environment_id=$1 AND id=$2`, env, id).Scan(&o.User, &o.Key, &o.Activation, &o.Agent, &o.Connection)
	return o, hideMissing(err)
}
func accessError(err error) error {
	if errors.Is(err, access.ErrDenied) || errors.Is(err, pgx.ErrNoRows) {
		return errors.Join(ErrDenied, err)
	}
	return err
}
func requirePermission(ctx context.Context, q db.DBTX, kind string, id, env, agent uuid.UUID, permission auth.Permission) error {
	return accessError(access.Check(ctx, q, kind, id, env, agent, permission))
}
func (o sessionOrigin) authorize(ctx context.Context, q db.DBTX, env, agent uuid.UUID) error {
	if o.mode() == "shared" {
		var allowed bool
		err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_connections c JOIN slack_installations i ON i.id=c.slack_installation_id JOIN slack_app_registrations r ON r.id=c.slack_app_registration_id WHERE c.id=$1 AND c.environment_id=$2 AND c.agent_id=$3 AND c.revoked_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.credential_ciphertext IS NOT NULL AND r.retired_at IS NULL AND r.credential_ciphertext IS NOT NULL)`, *o.Connection, env, agent).Scan(&allowed)
		if err != nil {
			return err
		}
		if !allowed {
			return ErrDenied
		}
		return nil
	}

	if o.kind() == "schedule" {
		// Activation intervals bound new occurrences, not the authority of Sessions
		// already admitted from them. Those Sessions retain their pinned deployment.
		// The Session's composite foreign key guarantees its origin exists in env.
		return nil
	}
	permission := auth.PermissionAgentsUse
	if o.mode() != "scoped" {
		return ErrDenied
	}
	return requirePermission(ctx, q, o.kind(), o.id(), env, agent, permission)
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
		o = sessionOrigin{Agent: agent}
		if caller.Kind == "user" {
			id := caller.ID
			o.User = &id
		} else if caller.Kind == "api_key" {
			id := caller.ID
			o.Key = &id
		} else {
			return o, ErrDenied
		}
	}
	if caller.Kind != "session" && caller.SlackConnectionID != uuid.Nil() {
		if err := requirePermission(ctx, tx, caller.Kind, caller.ID, req.EnvironmentID, agent, auth.PermissionAgentsUse); err != nil {
			return o, err
		}
		connection := caller.SlackConnectionID
		o.Connection = &connection
	}
	if err := o.authorize(ctx, tx, req.EnvironmentID, agent); err != nil {
		return o, err
	}
	if req.ComputerID != uuid.Nil() && o.mode() != "environment" {
		if o.mode() != "scoped" {
			return o, ErrDenied
		}
		if err := requirePermission(ctx, tx, o.kind(), o.id(), req.EnvironmentID, uuid.Nil(), auth.PermissionComputersExecute); err != nil {
			return o, err
		}
	}
	if o.mode() == "scoped" {
		if req.SessionKey != nil || req.SlackChannelID != nil {
			if err := requirePermission(ctx, tx, caller.Kind, caller.ID, req.EnvironmentID, agent, auth.PermissionSessionsCreateKeyed); err != nil {
				return o, err
			}
		}
		if o.kind() == "api_key" && req.IdempotencyKey == "" {
			return o, ErrInvalidInput
		}
	}
	return o, nil
}

// authorizeTarget is used before target reads, mutations and receipt returns.
// Runtime relations are additional constraints, never a replacement for origin.
func authorizeTarget(ctx context.Context, tx pgx.Tx, caller Caller, env, target uuid.UUID, operation auth.Permission) error {
	if err := authorizeTargetAuthority(ctx, tx, caller, env, target, operation); err != nil {
		return err
	}
	if auth.SessionExecutionOperation(operation) {
		o, err := loadSessionOrigin(ctx, tx, env, target)
		if err != nil {
			return err
		}
		if err := requireAgentAvailable(ctx, tx, env, o.Agent); err != nil {
			return err
		}
	}
	return nil
}

func authorizeTargetAuthority(ctx context.Context, tx pgx.Tx, caller Caller, env, target uuid.UUID, operation auth.Permission) error {
	o, err := loadSessionOrigin(ctx, tx, env, target)
	if err != nil {
		return err
	}
	if caller.Kind != "session" {
		p, err := access.Principal(ctx, tx, caller.Kind, caller.ID, env)
		if err != nil {
			return accessError(err)
		}
		if caller.Kind == "api_key" && caller.CredentialVersion > 0 && caller.CredentialVersion != p.CredentialVersion {
			return accessError(&access.PrincipalUnavailable{APIKeyID: caller.ID, Reason: "credential_version_changed", PresentedVersion: caller.CredentialVersion, ObservedVersion: p.CredentialVersion})
		}
		scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, AgentID: o.Agent.String()}
		if p.HasPermission(operation, scope) {
			if auth.SessionExecutionOperation(operation) {
				return o.authorize(ctx, tx, env, o.Agent)
			}
			return nil
		}
		if ownSessionOperation(operation) && o.mode() == "shared" && caller.SlackThreadID != uuid.Nil() && p.HasPermission(auth.PermissionAgentsUse, scope) {
			var related bool
			err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions s JOIN slack_threads t ON t.environment_id=s.environment_id AND t.front_session_id=s.root_session_id JOIN slack_channel_bindings b ON b.id=t.slack_channel_binding_id JOIN slack_user_links l ON l.team_id=t.team_id AND l.user_id=$4 WHERE s.environment_id=$1 AND s.id=$2 AND t.id=$3 AND t.deleted_at IS NULL AND b.connection_id=s.origin_slack_connection_id AND left(t.slack_channel_id,1)<>'D')`, env, target, caller.SlackThreadID, caller.ID).Scan(&related)
			if err != nil {
				return err
			}
			if related {
				return o.authorize(ctx, tx, env, o.Agent)
			}
		}
		if !ownSessionOperation(operation) || o.mode() != "scoped" || o.kind() != caller.Kind || o.id() != caller.ID {
			return auth.Require(p, operation, scope, ErrDenied)
		}
		if permissionErr := auth.Require(p, auth.PermissionAgentsUse, scope,
			ErrDenied); permissionErr != nil {
			return permissionErr
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
	if source.mode() == "environment" {
		if o.mode() != "environment" {
			return ErrDenied
		}
		return nil
	}
	if source.mode() == "shared" {
		if o.mode() != "shared" || *source.Connection != *o.Connection {
			return ErrDenied
		}
	} else if o.mode() != "scoped" || o.kind() != source.kind() || o.id() != source.id() {
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
	if err := requireAgentAvailable(ctx, tx, env, o.Agent); err != nil {
		return err
	}
	return o.authorize(ctx, tx, env, o.Agent)
}

type sessionReadScope struct {
	MCPAgentKeys     []string
	All, AllAgents   bool
	User, Key        *uuid.UUID
	Agents           []uuid.UUID
	ViewAgents       []uuid.UUID
	SharedConnection uuid.UUID
	// MembersRead says whether the caller may see the organization's headcount.
	MembersRead bool
}

func readScopeFor(ctx context.Context, q db.DBTX, caller Caller, env uuid.UUID) (sessionReadScope, error) {
	filter := sessionReadScope{Agents: []uuid.UUID{}}
	if caller.Kind == "user" {
		filter.User = &caller.ID
	} else if caller.Kind == "api_key" {
		filter.Key = &caller.ID
	}
	p, err := access.Principal(ctx, q, caller.Kind, caller.ID, env)
	if err != nil {
		return filter, accessError(err)
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
	filter.All, filter.ViewAgents = p.AgentPermissionScope(auth.PermissionSessionsRead, scope)
	filter.AllAgents, filter.Agents = p.AgentPermissionScope(auth.PermissionAgentsUse, scope)
	filter.MembersRead = p.HasPermission(auth.PermissionMembersRead, auth.Scope{OrgID: p.OrgID})
	// An active principal with no grant gets an empty list, never an unfiltered one.
	return filter, nil
}
func authorizeSessionRead(ctx context.Context, tx pgx.Tx, caller Caller, env, id uuid.UUID) error {
	return authorizeTarget(ctx, tx, caller, env, id, auth.PermissionSessionsRead)
}

func ownSessionOperation(operation auth.Permission) bool {
	switch operation {
	case auth.PermissionSessionsRead, auth.PermissionSessionsSend, auth.PermissionSessionsClose, auth.PermissionSessionsCancel, auth.PermissionSessionsInterrupt, auth.PermissionSessionsResume, auth.PermissionSessionsAnswer:
		return true
	}
	return false
}

// Recheck the immutable Session origin and the caller that supplied deferred input.
func authorizeDeferredInput(ctx context.Context, tx pgx.Tx, env, session uuid.UUID, caller Caller, input uuid.UUID, message bool, check inputAuthorityCheck) error {
	if caller.Kind == "user" {
		var request uuid.UUID
		err := tx.QueryRow(ctx, `SELECT id,thread_id FROM slack_requests WHERE environment_id=$1 AND session_id=$2 AND user_id=$3 AND status='accepted' AND (($5 AND message_id=$4) OR (NOT $5 AND turn_id=$4 AND message_id IS NULL)) AND thread_id IS NOT NULL`, env, session, caller.ID, input, message).Scan(&request, &caller.SlackThreadID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err == nil {
			if request != check.request {
				return ErrNotReady
			}
			if !check.allowed {
				return ErrDenied
			}
			if err := authorizeSlackInputActor(ctx, tx, request); err != nil {
				return err
			}
		}
	}
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
	if o.mode() == "environment" {
		return true, []uuid.UUID{}, nil
	}
	if o.mode() == "shared" {
		return false, []uuid.UUID{o.Agent}, nil
	}
	scope, err := readScopeFor(ctx, tx, Caller{Kind: o.kind(), ID: o.id()}, caller.Execution.EnvironmentID)
	return scope.AllAgents, scope.Agents, err
}

func failQueuedPermission(ctx context.Context, auditPolicy auditpolicy.Policy, tx pgx.Tx, env, session uuid.UUID) error {
	rows, err := tx.Query(ctx, `UPDATE turns SET status='failed',terminal_at=clock_timestamp(),processing_closed_at=clock_timestamp(),error_code='permission_revoked',error_message=$3 WHERE environment_id=$1 AND session_id=$2 AND status='queued' RETURNING id`, env, session, []byte("Permission was revoked before execution"))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = auditServiceFact(ctx, tx, auditPolicy, audit.ControlPlane, audit.Event{EnvironmentID: env, SessionID: session, TurnID: id, Action: "turn.failed", Outcome: "failed", TargetKind: "turn", TargetID: id, Details: audit.FailureDetails{Reason: "permission_denied"}}); err != nil {
			return err
		}
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
	own := o.mode() == "scoped" && o.kind() == caller.Kind && o.id() == caller.ID && p.HasPermission(auth.PermissionAgentsUse, scope)
	originAllowed := authorizeRuntimeOrigin(ctx, tx, env, id)
	if originAllowed != nil && !errors.Is(originAllowed, ErrDenied) && !errors.Is(originAllowed, ErrAgentDeleted) {
		return nil, originAllowed
	}
	for _, operation := range auth.AllPermissions() {
		if ownSessionOperation(operation) && (p.HasPermission(operation, scope) || own) && (!auth.SessionExecutionOperation(operation) || originAllowed == nil) {
			result = append(result, string(operation))
		}
	}
	return result, nil
}

// AuthorizeSessionOperation applies the same target checks to transport-owned
// presentation and local controls as to core Session actions.
func AuthorizeSessionOperation(ctx context.Context, tx pgx.Tx, caller Caller, env, target uuid.UUID, operation auth.Permission) error {
	return authorizeTarget(ctx, tx, caller, env, target, operation)
}
