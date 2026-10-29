package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/session"
	"github.com/jackc/pgx/v5"
)

// linkedHuman retains the live link and membership locks through admission.
// Action owners separately check the exact Session and operation.
func linkedHuman(ctx context.Context, tx pgx.Tx, team, actor string, installation, environment, agent uuid.UUID, source time.Time) (auth.Principal, string, error) {
	var link uuid.UUID
	var linked time.Time
	err := tx.QueryRow(ctx, `SELECT id,linked_at FROM slack_user_links WHERE installation_id=$1 AND team_id=$2 AND slack_user_id=$3 AND revoked_at IS NULL`, installation, team, actor).Scan(&link, &linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.Principal{}, "identity_unlinked", nil
	}
	if err != nil {
		return auth.Principal{}, "", err
	}
	if source.Before(linked) {
		return auth.Principal{}, "identity_unlinked", nil
	}
	p, err := access.SlackPrincipal(ctx, tx, link)
	if err == nil {
		p, err = access.HumanInEnvironment(ctx, tx, p, environment)
	}
	if errors.Is(err, access.ErrDenied) {
		return p, "permission_denied", nil
	}
	return p, "", err
}
func linkedSessionHuman(ctx context.Context, tx pgx.Tx, team, actor string, installation, environment, target uuid.UUID, source time.Time) (auth.Principal, string, error) {
	var agent uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT agent_id FROM sessions WHERE environment_id=$1 AND id=$2`, environment, target).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return auth.Principal{}, "permission_denied", nil
		}
		return auth.Principal{}, "", err
	}
	return linkedHuman(ctx, tx, team, actor, installation, environment, agent, source)
}
func slackCaller(p auth.Principal, thread uuid.UUID) session.Caller {
	c := session.UserCaller(p)
	c.SlackThreadID = thread
	return c
}

// Direct output remains bound to its original linked recipient and current use
// permission. A route that remains connected does not preserve a revoked user's
// access. Public channel output follows the configured shared publication route.
func directRecipient(ctx context.Context, tx pgx.Tx, thread uuid.UUID) (bool, auth.Principal, uuid.UUID, error) {
	var direct bool
	var link *uuid.UUID
	var env uuid.UUID
	err := tx.QueryRow(ctx, `SELECT left(t.slack_channel_id,1)='D',l.id,s.environment_id
 FROM slack_threads t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id)
 LEFT JOIN slack_user_links l ON l.id=s.human_slack_link_id AND l.user_id=s.origin_user_id
 AND l.team_id=t.recipient_team_id AND l.slack_user_id=t.recipient_user_id
 WHERE t.id=$1 AND (s.origin_slack_connection_id IS NULL OR left(t.slack_channel_id,1)<>'D')`, thread).Scan(&direct, &link, &env)
	if err != nil {
		return direct, auth.Principal{}, env, err
	}
	if !direct {
		return false, auth.Principal{}, env, nil
	}
	if link == nil {
		return true, auth.Principal{}, env, access.ErrDenied
	}
	p, err := access.SlackPrincipal(ctx, tx, *link)
	if err == nil {
		p, err = access.HumanInEnvironment(ctx, tx, p, env)
	}
	return true, p, env, err
}
func directRecipientAuthorized(ctx context.Context, tx pgx.Tx, thread uuid.UUID) (bool, error) {
	direct, p, env, err := directRecipient(ctx, tx, thread)
	if errors.Is(err, access.ErrDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !direct {
		return true, nil
	}
	var agent uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT s.agent_id FROM slack_threads t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) WHERE t.id=$1`, thread).Scan(&agent); err != nil {
		return false, err
	}
	scope := auth.Scope{OrgID: p.OrgID, ProjectID: p.ProjectID, EnvironmentID: env.String(), AgentID: agent.String()}
	return p.HasPermission(auth.PermissionAgentsUse, scope), nil
}
func directRecipientCanReadSession(ctx context.Context, tx pgx.Tx, thread, target uuid.UUID) (bool, error) {
	direct, p, env, err := directRecipient(ctx, tx, thread)
	if errors.Is(err, access.ErrDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !direct {
		return true, nil
	}
	err = session.AuthorizeSessionOperation(ctx, tx, session.UserCaller(p), env, target, auth.PermissionSessionsRead)
	if errors.Is(err, session.ErrDenied) {
		return false, nil
	}
	return err == nil, err
}

func directRecipientPostAuthorized(ctx context.Context, tx pgx.Tx, thread, post uuid.UUID) (bool, error) {
	allowed, err := directRecipientAuthorized(ctx, tx, thread)
	if err != nil || !allowed {
		return allowed, err
	}
	rows, err := tx.Query(ctx, `SELECT session_id FROM slack_posts WHERE id=$1 UNION SELECT session_id FROM slack_work_items WHERE post_id=$1`, post)
	if err != nil {
		return false, err
	}
	targets, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		allowed, err = directRecipientCanReadSession(ctx, tx, thread, target)
		if err != nil || !allowed {
			return allowed, err
		}
	}
	return true, nil
}
