package session

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"uuid"
)

// UserCaller carries the exact admitted source through operation boundaries.
func UserCaller(p auth.Principal) Caller {
	c := Caller{Kind: "user", ID: p.UserID, Human: p, CredentialKind: "login_session", CredentialID: p.SessionID}
	if p.SlackLinkID != uuid.Nil() {
		c.CredentialKind = "slack_link"
		c.CredentialID = p.SlackLinkID
	}
	return c
}
func callerPrincipal(ctx context.Context, q db.DBTX, c Caller, env uuid.UUID) (auth.Principal, error) {
	if c.Kind == "user" {
		if c.Human.UserID != c.ID {
			return auth.Principal{}, access.ErrDenied
		}
		return access.HumanInEnvironment(ctx, q, c.Human, env)
	}
	return access.Principal(ctx, q, c.Kind, c.ID, env)
}
func requireCallerPermission(ctx context.Context, q db.DBTX, c Caller, env, agent uuid.UUID, permission auth.Permission) error {
	p, err := callerPrincipal(ctx, q, c, env)
	if err != nil {
		return accessError(err)
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID}
	if agent != uuid.Nil() {
		scope.AgentID = agent.String()
	}
	return auth.Require(p, permission, scope, ErrDenied)
}
