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
		if c.admitted {
			return access.WorkInEnvironment(ctx, q, c.Human, env)
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

func saveSessionHuman(ctx context.Context, q db.DBTX, env, id uuid.UUID, p auth.Principal) error {
	_, err := q.Exec(ctx, `UPDATE sessions SET (`+access.HumanColumns+`)=($3,$4,$5,$6,$7,$8,$9,$10,$11) WHERE environment_id=$1 AND id=$2`, append([]any{env, id}, access.HumanValues(p)...)...)
	return err
}
func saveTurnHuman(ctx context.Context, q db.DBTX, env, id uuid.UUID, c Caller) error {
	if c.Kind != "user" {
		return nil
	}
	_, err := q.Exec(ctx, `UPDATE turns SET (`+access.HumanColumns+`)=($3,$4,$5,$6,$7,$8,$9,$10,$11) WHERE environment_id=$1 AND id=$2`, append([]any{env, id}, access.HumanValues(c.Human)...)...)
	return err
}
func saveMessageHuman(ctx context.Context, q db.DBTX, env, id uuid.UUID, c Caller) error {
	if c.Kind != "user" {
		return nil
	}
	_, err := q.Exec(ctx, `UPDATE turn_messages SET (`+access.HumanColumns+`)=($3,$4,$5,$6,$7,$8,$9,$10,$11) WHERE environment_id=$1 AND id=$2`, append([]any{env, id}, access.HumanValues(c.Human)...)...)
	return err
}
