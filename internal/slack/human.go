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
func linkedHuman(ctx context.Context, tx pgx.Tx, team, actor string, environment, agent uuid.UUID, source time.Time) (uuid.UUID, string, error) {
	var user uuid.UUID
	var linked time.Time
	err := tx.QueryRow(ctx, `SELECT user_id,linked_at FROM slack_user_links WHERE team_id=$1 AND slack_user_id=$2 FOR SHARE`, team, actor).Scan(&user, &linked)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil(), "identity_unlinked", nil
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	if source.Before(linked) {
		return uuid.Nil(), "identity_unlinked", nil
	}
	_, err = access.Principal(ctx, tx, "user", user, environment)
	if errors.Is(err, access.ErrDenied) {
		return uuid.Nil(), "permission_denied", nil
	}
	if err != nil {
		return uuid.Nil(), "", err
	}
	return user, "", nil
}

func linkedSessionHuman(ctx context.Context, tx pgx.Tx, team, actor string, environment, target uuid.UUID, source time.Time) (uuid.UUID, string, error) {
	var agent uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT agent_id FROM sessions WHERE environment_id=$1 AND id=$2`, environment, target).Scan(&agent); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil(), "permission_denied", nil
		}
		return uuid.Nil(), "", err
	}
	return linkedHuman(ctx, tx, team, actor, environment, agent, source)
}

// Direct output remains bound to its original linked recipient and current use
// permission. A route that remains connected does not preserve a revoked user's
// access. Public channel output follows the configured shared publication route.
func directRecipientAuthorized(ctx context.Context, tx pgx.Tx, thread uuid.UUID) (bool, error) {
	var direct, bound bool
	var user *uuid.UUID
	var env, agent uuid.UUID
	err := tx.QueryRow(ctx, `SELECT left(t.slack_channel_id,1)='D',l.user_id,s.environment_id,s.agent_id,
 s.origin_slack_connection_id IS NULL AND COALESCE(s.origin_user_id=l.user_id,false)
 FROM slack_threads t JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id)
 LEFT JOIN slack_user_links l ON l.team_id=t.recipient_team_id AND l.slack_user_id=t.recipient_user_id
 WHERE t.id=$1`, thread).Scan(&direct, &user, &env, &agent, &bound)
	if err != nil {
		return false, err
	}
	if !direct {
		return true, nil
	}
	if user == nil || !bound {
		return false, nil
	}
	err = access.Check(ctx, tx, "user", *user, env, agent, auth.PermissionAgentsUse)
	if errors.Is(err, access.ErrDenied) {
		return false, nil
	}
	return err == nil, err
}

// Child activity is separate Agent metadata even when attached to a parent's
// work message. Channel publication follows its shared route; DMs require the
// recipient's current access to every represented Session.
func directRecipientCanReadSession(ctx context.Context, tx pgx.Tx, thread, target uuid.UUID) (bool, error) {
	var direct bool
	var user *uuid.UUID
	var env uuid.UUID
	err := tx.QueryRow(ctx, `SELECT left(t.slack_channel_id,1)='D',l.user_id,t.environment_id
 FROM slack_threads t LEFT JOIN slack_user_links l ON l.team_id=t.recipient_team_id AND l.slack_user_id=t.recipient_user_id
 WHERE t.id=$1`, thread).Scan(&direct, &user, &env)
	if err != nil {
		return false, err
	}
	if !direct {
		return true, nil
	}
	if user == nil {
		return false, nil
	}
	err = session.AuthorizeSessionOperation(ctx, tx, session.Caller{Kind: "user", ID: *user}, env, target, auth.PermissionSessionsRead)
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
