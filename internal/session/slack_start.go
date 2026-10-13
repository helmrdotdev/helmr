package session

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/slackroute"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/definition"
	"github.com/jackc/pgx/v5"
)

var (
	ErrSlackChannelUnavailable       = errors.New("the Slack channel is unavailable; check that both apps can access this channel")
	ErrSlackDestinationConflict      = errors.New("session Slack destination conflicts")
	ErrSourceConversationPending     = errors.New("source conversation is still being delivered; retry after its Slack thread is confirmed")
	ErrSourceConversationUnavailable = errors.New("source Slack conversation is unavailable")
	ErrTargetWorkspaceMismatch       = errors.New("target Agent is connected to a different Slack workspace")
	ErrStartMessageTooLarge          = errors.New("start input and attribution must fit in one Slack message")
)

// slackRouteOf returns the channel of the Slack route a root Session owns, or
// nil when the Session owns none. The thread row is the route authority.
func slackRouteOf(ctx context.Context, tx pgx.Tx, env, session uuid.UUID) (*uuid.UUID, error) {
	var route uuid.UUID
	err := tx.QueryRow(ctx, `SELECT channel_id FROM slack_threads WHERE environment_id=$1 AND front_session_id=$2`, env, session).Scan(&route)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &route, nil
}

func readSlackStartSource(ctx context.Context, tx pgx.Tx, env, caller uuid.UUID) (*slackroute.StartSource, error) {
	route, err := slackRouteOf(ctx, tx, env, caller)
	if err != nil || route == nil {
		return nil, err
	}
	source := &slackroute.StartSource{SessionID: caller, RouteID: *route}
	c := &source.Connection
	c.EnvironmentID = env
	var root *string
	var active, abandoned bool
	err = tx.QueryRow(ctx, `SELECT t.id,t.thread_ts,c.connection_id,r.id,i.id,c.org_id,c.team_id,c.slack_channel_id,i.app_id,i.bot_user_id,s.agent_id,a.key,i.credential_revision,i.authorized_at,
 p.revoked_at IS NULL AND r.retired_at IS NULL AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND t.deleted_at IS NULL,
 EXISTS(SELECT 1 FROM slack_posts post WHERE post.thread_id=t.id AND post.role='opening' AND (post.delivery_disposed_at IS NOT NULL OR (t.thread_ts IS NULL AND post.status IN ('failed','suppressed'))))
 FROM slack_threads t JOIN slack_channel_bindings c ON c.id=t.channel_id JOIN slack_connections p ON p.id=c.connection_id
 JOIN slack_installations i ON i.id=c.installation_id JOIN slack_app_registrations r ON r.id=i.app_registration_id
 JOIN sessions s ON (s.environment_id,s.id)=(t.environment_id,t.front_session_id) JOIN agents a ON (a.environment_id,a.id)=(s.environment_id,s.agent_id)
 WHERE t.environment_id=$1 AND t.front_session_id=$2 AND t.channel_id=$3`, env, caller, *route).Scan(&source.ThreadID, &root, &c.ConnectionID, &c.RegistrationID, &c.InstallationID, &c.OrganizationID, &c.TeamID, &c.ChannelID, &c.AppID, &c.BotUserID, &c.AgentID, &c.AgentName, &c.CredentialRevision, &c.AuthorizedAt, &active, &abandoned)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSourceConversationUnavailable
	}
	if err != nil {
		return nil, err
	}
	if !active || abandoned {
		return nil, ErrSourceConversationUnavailable
	}
	if root == nil {
		return nil, ErrSourceConversationPending
	}
	source.ThreadTS = *root
	return source, nil
}

// prepareStart returns an authorized exact receipt before live promotion or
// connection checks. All remote work happens after this transaction ends.
func prepareStart(ctx context.Context, pool db.TxBeginner, caller Caller, req StartRequest, method string, digest [32]byte) (*Admission, *slackroute.StartRoute, error) {
	if req.PreparedSlack != nil {
		return nil, req.PreparedSlack, nil
	}
	var receipt *Admission
	var route *slackroute.StartRoute
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if caller.Kind == "session" {
			if err := lockRuntimeHost(ctx, tx, caller); err != nil {
				return err
			}
			if err := requireRootCaller(ctx, tx, caller); err != nil {
				return err
			}
		} else if err := authorizeStart(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		var agent, deployment uuid.UUID
		var err error
		if caller.Kind == "session" && method == "spawn" {
			err = tx.QueryRow(ctx, `SELECT d.agent_id,s.deployment_id FROM sessions s JOIN agent_definitions d ON (d.environment_id,d.deployment_id)=(s.environment_id,s.deployment_id) JOIN agents a ON (a.environment_id,a.id)=(d.environment_id,d.agent_id) WHERE s.environment_id=$1 AND s.id=$2 AND a.key=$3`, req.EnvironmentID, caller.ID, req.Agent).Scan(&agent, &deployment)
		} else {
			err = tx.QueryRow(ctx, `SELECT id FROM agents WHERE environment_id=$1 AND key=$2`, req.EnvironmentID, req.Agent).Scan(&agent)
		}
		if err != nil {
			return err
		}
		receipt, err = sessionCreationReceipt(ctx, tx, caller, req.EnvironmentID, agent, method, req.IdempotencyKey, digest)
		if err != nil || receipt != nil {
			return err
		}

		if caller.Kind == "session" {
			owner, e := lockSession(ctx, tx, req.EnvironmentID, caller.ID)
			if e != nil {
				return e
			}
			if e = authorizeEnqueue(ctx, tx, caller, req.EnvironmentID, owner); e != nil {
				return e
			}
		}
		if method == "spawn" {
			return nil
		}
		if req.SessionKey != nil {
			var occupied bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE environment_id=$1 AND agent_id=$2 AND key=$3)`, req.EnvironmentID, agent, *req.SessionKey).Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return ErrConflict
			}
		}

		var source *slackroute.StartSource
		channel := ""
		if caller.Kind == "session" {
			source, err = readSlackStartSource(ctx, tx, req.EnvironmentID, caller.ID)
			if err != nil {
				return err
			}
			if source != nil {
				channel = source.Connection.ChannelID
			}
		}
		if req.SlackChannelID != nil {
			channel = *req.SlackChannelID
		}
		if channel == "" {
			return nil
		}
		if err = tx.QueryRow(ctx, `SELECT current_deployment_id FROM environments WHERE id=$1 AND current_deployment_id IS NOT NULL`, req.EnvironmentID).Scan(&deployment); err != nil {
			return ErrNotReady
		}
		target, err := slackroute.ReadConnection(ctx, tx, req.EnvironmentID, agent, channel)
		if err != nil {
			return err
		}
		if source != nil && (source.Connection.TeamID != target.TeamID || source.Connection.OrganizationID != target.OrganizationID) {
			return ErrTargetWorkspaceMismatch
		}
		route = &slackroute.StartRoute{Target: target, DeploymentID: deployment, Source: source}
		return nil
	})
	if err != nil {
		return nil, nil, hideMissing(err)
	}
	if receipt != nil || route == nil {
		return receipt, route, nil
	}
	if req.SlackPreflight == nil {
		return nil, nil, ErrSlackChannelUnavailable
	}
	route.Opening, err = req.SlackPreflight.PrepareStart(ctx, slackroute.StartContext{Route: *route, Input: req.Input})
	if err != nil {
		return nil, nil, err
	}
	return nil, route, nil
}

func validateSlackStart(ctx context.Context, tx pgx.Tx, env, agent, deployment uuid.UUID, caller Caller, p *slackroute.StartRoute) error {
	if p == nil {
		return nil
	}
	if p.Target.EnvironmentID != env || p.Target.AgentID != agent || p.DeploymentID != deployment || !definition.ValidSlackChannelID(p.Target.ChannelID) {
		return slackroute.ErrConversationChanged
	}
	c, err := slackroute.ReadConnection(ctx, tx, env, agent, p.Target.ChannelID)
	if err != nil {
		return slackroute.ErrConversationChanged
	}
	if c.ConnectionID != p.Target.ConnectionID || c.RegistrationID != p.Target.RegistrationID || c.InstallationID != p.Target.InstallationID || c.TeamID != p.Target.TeamID || c.OrganizationID != p.Target.OrganizationID || !c.AuthorizedAt.Equal(p.Target.AuthorizedAt) {
		return slackroute.ErrConversationChanged
	}
	if caller.Kind == "session" {
		if p.HumanThreadTS != "" {
			return ErrDenied
		}
		source, err := readSlackStartSource(ctx, tx, env, caller.ID)
		if err != nil {
			return err
		}
		if (source == nil) != (p.Source == nil) || source != nil && (source.ThreadID != p.Source.ThreadID || source.RouteID != p.Source.RouteID || source.ThreadTS != p.Source.ThreadTS || !source.Connection.AuthorizedAt.Equal(p.Source.Connection.AuthorizedAt)) {
			return slackroute.ErrConversationChanged
		}
	}
	if p.Source != nil && (p.Source.Connection.TeamID != c.TeamID || p.Source.Connection.ChannelID != c.ChannelID || p.Source.Connection.OrganizationID != c.OrganizationID) {
		return slackroute.ErrConversationChanged
	}
	if p.HumanThreadTS == "" && (len(p.Opening) == 0 || !json.Valid(p.Opening)) {
		return ErrInvalidInput
	}
	return nil
}

func createSlackRoot(ctx context.Context, tx pgx.Tx, env, session, turn, channel uuid.UUID, p slackroute.StartRoute) error {
	thread := uuid.NewV7()
	var opening, root, recipientTeam, recipientUser any
	if p.HumanThreadTS != "" {
		root = p.HumanThreadTS
		if p.HumanActor != "" {
			recipientTeam = p.Target.TeamID
			recipientUser = p.HumanActor
		}
	} else {
		opening = "opening:" + session.String()
	}
	_, err := tx.Exec(ctx, `INSERT INTO slack_threads(id,environment_id,front_session_id,channel_id,org_id,team_id,slack_channel_id,thread_ts,opening_post_key,recipient_team_id,recipient_user_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, thread, env, session, channel, p.Target.OrganizationID, p.Target.TeamID, p.Target.ChannelID, root, opening, recipientTeam, recipientUser)
	if err != nil {
		return err
	}
	source, err := slackroute.AddMember(ctx, tx, env, thread, session)
	if err != nil {
		return err
	}
	if p.HumanThreadTS != "" {
		return nil
	}
	var sourceThread any
	if p.Source != nil {
		sourceThread = p.Source.ThreadID
	}
	digest := sha256.Sum256(p.Opening)
	_, err = tx.Exec(ctx, `INSERT INTO slack_posts(id,environment_id,session_id,thread_source_id,thread_id,source_thread_id,seq,post_key,role,turn_id,payload,payload_digest,presentation_path,closed_at) VALUES($1,$2,$3,$4,$5,$6,1,$7,'opening',$8,$9,$10,'post',clock_timestamp())`, uuid.NewV7(), env, session, source, thread, sourceThread, opening, nullableID(turn), []byte(p.Opening), digest[:])
	return err
}
